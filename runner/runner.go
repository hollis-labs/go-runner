package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// Config configures a single Run. Provider, Workspace, and OnEvent are
// required; Profile is optional (zero-value skips sandbox wrapping). Args
// are the raw CLI argv (the runner does not call adapter.BuildArgs — that
// is the wrapper's job). Env defaults to the parent process environment
// when nil; an explicit empty slice yields an empty environment.
type Config struct {
	// Provider is consulted for binary detection and per-line parsing.
	Provider provider.CLIAdapter

	// Profile is the sandbox profile to apply. The zero value (empty ID)
	// disables sandboxing — the cmd is spawned without sandbox-exec/bwrap.
	Profile sandbox.Profile

	// Workspace is the absolute path used as the cmd working directory and
	// passed to sandbox.Apply when Profile is non-zero.
	Workspace string

	// Args is the CLI argv (after the binary). Pass adapter.BuildArgs(...)
	// here if the wrapper needs adapter-specific arg construction.
	Args []string

	// Env is the cmd environment. nil inherits the parent env via
	// os/exec defaults; an empty non-nil slice disables inheritance.
	Env []string

	// Stderr, when non-nil, is wired to cmd.Stderr before spawn. The
	// runner does not interpret stderr — bytes flow through verbatim.
	// Use io.MultiWriter to fan out (e.g. an in-memory buffer plus a
	// sidecar log file). Nil leaves cmd.Stderr unset, which os/exec
	// routes to os.DevNull.
	//
	// When Supervisor is non-nil, the runner wraps cfg.Stderr with an
	// activity tap so byte-level stderr writes count as supervisor
	// activity. The wrap is transparent to the caller (writes flow
	// through; bytes are unchanged).
	Stderr io.Writer

	// ExtraFiles is the additional set of open files to be inherited by
	// the spawned process. The first entry maps to FD 3, the second to
	// FD 4, and so on (per os/exec.Cmd.ExtraFiles semantics). Used by
	// consumers that need to plumb out-of-band channels into the spawned
	// binary. Nil leaves the default empty.
	ExtraFiles []*os.File

	// WaitDelay is the grace period between SIGTERM (on context cancel)
	// and SIGKILL. Zero falls through to provider.DefaultWaitDelay.
	WaitDelay time.Duration

	// Supervisor, when non-nil, enables process supervision: idle-kill,
	// restart-on-crash, and watchdog. See SupervisorOptions. Nil
	// preserves go-runner's default "spawn once, run to completion"
	// behavior. Added in v0.3.0.
	Supervisor *SupervisorOptions

	// ResourceLimits applies OS-level resource caps to the spawned
	// process via sh -c ulimit wrapping (both platforms) and / or
	// systemd-run --user --scope (Linux when systemd is available).
	// Zero value disables limits. See ResourceLimits. Added in v0.3.0.
	ResourceLimits ResourceLimits

	// OnEvent receives each runner Event synchronously from the spawn
	// goroutine. Required.
	OnEvent func(Event)
}

// Run spawns cfg.Provider's binary under cfg.Profile, streams its stdout
// through the adapter, and emits runner Events via cfg.OnEvent until the
// process exits or the context is cancelled.
//
// Default behavior (cfg.Supervisor == nil): one spawn, run to completion,
// return. When cfg.Supervisor is non-nil, Run drives a supervision loop
// — idle-kill / watchdog observation and up to RestartOnCrash restart
// attempts on non-zero exit.
//
// Returns nil on clean exit. On non-clean exit returns an *ExitError
// (extractable via errors.As) carrying structured Code / Signal /
// Killed / Cause. Setup and validation failures (missing required
// Config fields, provider Detect failure, sandbox.Apply error,
// cmd.Start error) return before any Events are emitted.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Provider == nil {
		return errors.New("runner: Config.Provider is required")
	}
	if cfg.OnEvent == nil {
		return errors.New("runner: Config.OnEvent is required")
	}
	if cfg.Workspace == "" {
		return errors.New("runner: Config.Workspace is required")
	}

	if cfg.Supervisor != nil {
		return runSupervised(ctx, cfg)
	}
	return runOnce(ctx, cfg)
}

// runOnce executes a single subprocess lifetime: detect → spawn →
// stream → wait. When cfg.Supervisor is non-nil, idle-kill / watchdog
// goroutines are started after spawn and torn down after wait.
func runOnce(ctx context.Context, cfg Config) error {
	binPath, ok := cfg.Provider.Detect()
	if !ok {
		return fmt.Errorf("runner: provider %q binary not found", cfg.Provider.Name())
	}

	if cfg.WaitDelay > 0 {
		ctx = provider.WithWaitDelay(ctx, cfg.WaitDelay)
	}

	cmd := exec.CommandContext(ctx, binPath, cfg.Args...)
	cmd.Dir = cfg.Workspace
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	if len(cfg.ExtraFiles) > 0 {
		cmd.ExtraFiles = cfg.ExtraFiles
	}

	// Activity tracking: wire stderr tap (and tick from stdout scanner
	// below) so supervisor goroutines can observe I/O activity.
	var activity *activityTracker
	state := &supState{}
	if cfg.Supervisor != nil {
		activity = &activityTracker{}
		cmd.Stderr = installActivityTap(cfg.Stderr, activity)
		// Populate caller-facing ActivityCallback. Reads of this field
		// from caller's OnEvent will see the live trampoline by the
		// time the first event fires (Start happens-before OnEvent).
		cfg.Supervisor.ActivityCallback = activity.tick
	} else if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("runner: stdout pipe: %w", err)
	}

	cleanup := func() {}
	if cfg.Profile.ID != "" {
		c, err := sandbox.Apply(cmd, cfg.Profile, cfg.Workspace)
		if err != nil {
			return fmt.Errorf("runner: sandbox apply: %w", err)
		}
		cleanup = c
	}
	defer cleanup()

	// Resource limits wrap is applied AFTER sandbox so the rlimit-
	// setting shell exec's into the sandbox helper which exec's into
	// the real binary; rlimits propagate down the chain.
	limitCleanup, err := applyResourceLimits(cmd, cfg.ResourceLimits)
	if err != nil {
		return fmt.Errorf("runner: apply resource limits: %w", err)
	}
	defer limitCleanup()

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = provider.WaitDelayFromContext(ctx)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("runner: start: %w", err)
	}
	startedAt := time.Now()

	cfg.OnEvent(Event{
		Kind: EventProcessStarted,
		At:   startedAt,
		Payload: map[string]any{
			"pid":    cmd.Process.Pid,
			"binary": cmd.Path,
			"args":   append([]string(nil), cmd.Args...),
		},
	})

	procDone := make(chan struct{})
	var supWG sync.WaitGroup
	if cfg.Supervisor != nil {
		if cfg.Supervisor.IdleKill > 0 {
			supWG.Add(1)
			go func() {
				defer supWG.Done()
				superviseIdle(cfg, cmd, activity, state, startedAt, procDone)
			}()
		}
		if cfg.Supervisor.WatchdogTimeout > 0 {
			supWG.Add(1)
			go func() {
				defer supWG.Done()
				superviseWatchdog(cfg, cmd, activity, state, startedAt, procDone)
			}()
		}
	}

	streamProviderEvents(stdout, cfg, activity)

	waitErr := cmd.Wait()
	close(procDone)
	supWG.Wait()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		cfg.OnEvent(Event{
			Kind:    EventProcessTimeout,
			At:      time.Now(),
			Payload: map[string]any{"error": ctx.Err().Error()},
		})
		return waitErr
	default:
		xe := buildExitError(cmd.ProcessState, waitErr, state.getCause())
		errText := ""
		if waitErr != nil {
			errText = waitErr.Error()
		}
		payload := map[string]any{
			"exit_code": -1,
			"signal":    0,
			"killed":    false,
			"cause":     "",
			"error":     errText,
		}
		if xe != nil {
			payload["exit_code"] = xe.Code
			payload["signal"] = xe.Signal
			payload["killed"] = xe.Killed
			payload["cause"] = xe.Cause
		} else {
			payload["exit_code"] = 0
		}
		cfg.OnEvent(Event{
			Kind:    EventProcessExited,
			At:      time.Now(),
			Payload: payload,
		})
		if xe != nil {
			return xe
		}
		return waitErr
	}
}

// buildExitError translates the (ProcessState, waitErr) pair returned by
// cmd.Wait into a structured *ExitError, or nil for clean exits. cause
// is set by the supervisor / resource-limits subsystems when they
// triggered the termination directly; pass empty string for ordinary
// exits.
func buildExitError(ps *os.ProcessState, waitErr error, cause string) *ExitError {
	if waitErr == nil {
		return nil
	}
	xe := &ExitError{
		Code:         -1,
		ProcessState: ps,
		Cause:        cause,
		waitErr:      waitErr,
	}
	if ps != nil {
		xe.Code = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				sig := int(ws.Signal())
				xe.Signal = sig
				xe.Killed = ws.Signal() == syscall.SIGKILL
			}
		}
	}
	var ee *exec.ExitError
	if xe.Code == -1 && errors.As(waitErr, &ee) {
		xe.Code = ee.ExitCode()
	}
	return xe
}

// streamProviderEvents reads stdout line-by-line, runs each line through the
// adapter, and emits one EventProviderEvent per parsed StreamEvent. Parse
// errors are silently dropped to match go-providers' bridge behavior; the
// adapter is the authority on what counts as a parseable line.
//
// When activity is non-nil (supervision active), every stdout line ticks
// the activity tracker, regardless of whether the adapter parsed it. This
// is the fallback signal used when the caller does not invoke the
// supervisor's ActivityCallback.
func streamProviderEvents(stdout io.ReadCloser, cfg Config, activity *activityTracker) {
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if activity != nil {
			activity.tick()
		}
		if len(line) == 0 {
			continue
		}
		events, err := cfg.Provider.ParseLine(line)
		if err != nil {
			continue
		}
		for _, ev := range events {
			cfg.OnEvent(Event{
				Kind: EventProviderEvent,
				At:   time.Now(),
				Payload: map[string]any{
					"event":            ev,
					"is_turn_complete": llmtypes.IsTurnComplete(ev),
				},
			})
		}
	}
}
