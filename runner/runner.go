package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	llmtypes "github.com/hollis-labs/go-llm-types"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// Config configures a single Run. Provider, Workspace, and OnEvent are
// required. SandboxPolicy is the preferred resolved go-sandbox access policy;
// Profile is the legacy compatibility adapter. Args are the raw CLI argv (the
// runner does not call adapter.BuildArgs — that is the wrapper's job). Env
// defaults to the parent process environment when nil; an explicit empty slice
// yields an empty environment.
type Config struct {
	// Provider is consulted for binary detection and per-line parsing.
	Provider provider.CLIAdapter

	// SandboxPolicy is an already-resolved go-sandbox access policy to apply
	// before every child Start. When non-nil, the policy ID must be non-empty.
	// Required policy setup errors prevent Start. A policy with
	// ConfinementDisabled is reported as disabled and starts unwrapped.
	SandboxPolicy *sandbox.ResolvedAccessPolicy

	// Profile is the legacy sandbox profile to apply. The zero value (empty ID)
	// disables legacy sandbox wrapping. Legacy profiles are default-allow
	// compatibility policies; new callers should prefer SandboxPolicy.
	Profile sandbox.Profile

	// Workspace is the absolute path used as the cmd working directory. Legacy
	// Profile callers also pass it to sandbox.Apply. Resolved SandboxPolicy
	// callers should set Roots.CWD independently when cwd differs from Project.
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

// Run spawns cfg.Provider's binary under cfg.SandboxPolicy or cfg.Profile,
// streams its stdout through the adapter, and emits runner Events via
// cfg.OnEvent until the process exits or the context is cancelled.
//
// Default behavior (cfg.Supervisor == nil): one spawn, run to completion,
// return. When cfg.Supervisor is non-nil, Run drives a supervision loop
// — idle-kill / watchdog observation and up to RestartOnCrash restart
// attempts on non-zero exit. The same sandbox setup is repeated before
// every restart Start.
//
// Returns nil on clean exit. On non-clean exit returns an *ExitError
// (extractable via errors.As) carrying structured Code / Signal /
// Killed / Cause. Setup and validation failures (missing required
// Config fields, provider Detect failure, sandbox setup error,
// cmd.Start error) return before process lifecycle Events are emitted.
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
	if cfg.SandboxPolicy != nil && cfg.Profile.ID != "" {
		return errors.New("runner: Config.SandboxPolicy and Config.Profile are mutually exclusive")
	}
	if cfg.SandboxPolicy != nil && cfg.SandboxPolicy.ID == "" {
		return errors.New("runner: Config.SandboxPolicy.ID is required")
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

	sandboxResult, sandboxCleanup, err := prepareSandbox(cmd, cfg)
	if err != nil {
		return err
	}
	defer sandboxCleanup()

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
		return &StartError{Outcome: sandboxResult.Configured(), Err: fmt.Errorf("runner: start: %w", err)}
	}
	startedAt := time.Now()
	launchedSandbox := sandboxResult.Launched()

	cfg.OnEvent(Event{
		Kind: EventProcessStarted,
		At:   startedAt,
		Payload: map[string]any{
			"pid":     cmd.Process.Pid,
			"binary":  cmd.Path,
			"args":    append([]string(nil), cmd.Args...),
			"sandbox": launchedSandbox,
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
			Payload: map[string]any{"error": ctx.Err().Error(), "sandbox": launchedSandbox},
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
			"sandbox":   launchedSandbox,
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

type sandboxApplier func(*exec.Cmd, sandbox.ResolvedAccessPolicy) (sandbox.EnforcementOutcome, func(), error)
type legacySandboxApplier func(*exec.Cmd, sandbox.Profile, string) (func(), error)

var (
	applyResolvedSandbox sandboxApplier       = sandbox.ApplyResolved
	applyLegacySandbox   legacySandboxApplier = sandbox.Apply
)

// SandboxRunState is go-runner's lifecycle view of sandbox setup for one child
// process. It intentionally separates configured-before-Start from
// launched-after-Start because the OS guarantee is not observed until Start
// succeeds.
type SandboxRunState string

const (
	SandboxStateDisabled    SandboxRunState = "disabled"
	SandboxStateUnsupported SandboxRunState = "unsupported"
	SandboxStateConfigured  SandboxRunState = "configured"
	SandboxStateLaunched    SandboxRunState = "launched"
	SandboxStateFailed      SandboxRunState = "failed"
)

// SandboxOutcome is safe to place in runner Events and errors. It records only
// policy/backend state and diagnostics; argv, environment and secrets are not
// copied into the value.
type SandboxOutcome struct {
	PolicyID           string
	Mode               sandbox.ConfinementMode
	Backend            sandbox.BackendName
	State              SandboxRunState
	Enforced           bool
	Disabled           bool
	Legacy             bool
	LegacyDefaultAllow bool
	Unsupported        []string
	Diagnostics        []string
	BackendGOOS        string
	BackendReady       bool
}

// StartError wraps cmd.Start failures and carries the sandbox state that had
// been configured before the failed Start. It lets callers distinguish backend
// setup success from an actual launched child.
type StartError struct {
	Outcome SandboxOutcome
	Err     error
}

func (e *StartError) Error() string {
	if e == nil || e.Err == nil {
		return "runner: start"
	}
	return e.Err.Error()
}

func (e *StartError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SandboxError wraps sandbox setup failures. Unsupported backends/policies are
// reported separately from configured backend failures.
type SandboxError struct {
	Outcome SandboxOutcome
	Err     error
}

func (e *SandboxError) Error() string {
	if e == nil || e.Err == nil {
		return "runner: sandbox setup"
	}
	return e.Err.Error()
}

func (e *SandboxError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func prepareSandbox(cmd *exec.Cmd, cfg Config) (SandboxOutcome, func(), error) {
	if cfg.SandboxPolicy != nil {
		out, cleanup, err := applyResolvedSandbox(cmd, *cfg.SandboxPolicy)
		mapped := sandboxOutcomeFromResolved(out)
		if err != nil {
			mapped.State = classifyResolvedSandboxError(out, err)
			return mapped, func() {}, &SandboxError{Outcome: mapped, Err: fmt.Errorf("runner: sandbox apply resolved: %w", err)}
		}
		if cleanup == nil {
			cleanup = func() {}
		}
		return mapped.Configured(), cleanup, nil
	}

	if cfg.Profile.ID == "" {
		return SandboxOutcome{State: SandboxStateDisabled, Disabled: true}, func() {}, nil
	}

	cleanup, err := applyLegacySandbox(cmd, cfg.Profile, cfg.Workspace)
	legacy := SandboxOutcome{
		PolicyID:           cfg.Profile.ID,
		Mode:               sandbox.ConfinementRequired,
		Backend:            sandbox.BackendAuto,
		State:              SandboxStateConfigured,
		Enforced:           true,
		Legacy:             true,
		LegacyDefaultAllow: true,
		Diagnostics:        []string{"legacy Profile adapter: default-allow compatibility semantics"},
	}
	if err != nil {
		legacy.Enforced = false
		legacy.State = classifyLegacySandboxError(err)
		legacy.Diagnostics = append(legacy.Diagnostics, err.Error())
		return legacy, func() {}, &SandboxError{Outcome: legacy, Err: fmt.Errorf("runner: sandbox apply legacy profile: %w", err)}
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	return legacy, cleanup, nil
}

func sandboxOutcomeFromResolved(out sandbox.EnforcementOutcome) SandboxOutcome {
	mapped := SandboxOutcome{
		PolicyID:     out.PolicyID,
		Mode:         out.Mode,
		Backend:      out.Backend,
		Enforced:     out.Enforced,
		Disabled:     out.Disabled,
		Diagnostics:  append([]string(nil), out.Diagnostics...),
		BackendGOOS:  out.BackendGOOS,
		BackendReady: out.BackendReady,
	}
	for _, cap := range out.Unsupported {
		mapped.Unsupported = append(mapped.Unsupported, string(cap))
	}
	switch out.State {
	case sandbox.EnforcementDisabled:
		mapped.State = SandboxStateDisabled
	case sandbox.EnforcementUnsupported:
		mapped.State = SandboxStateUnsupported
	case sandbox.EnforcementConfigured, sandbox.EnforcementApplied:
		mapped.State = SandboxStateConfigured
	case sandbox.EnforcementFailed:
		mapped.State = SandboxStateFailed
	default:
		mapped.State = SandboxStateFailed
		if out.State != "" {
			mapped.Diagnostics = append(mapped.Diagnostics, "unknown sandbox enforcement state: "+string(out.State))
		}
	}
	return mapped
}

func classifyResolvedSandboxError(out sandbox.EnforcementOutcome, err error) SandboxRunState {
	if out.State == sandbox.EnforcementUnsupported || errors.Is(err, sandbox.ErrBackendUnavailable) || errors.Is(err, sandbox.ErrUnsupportedPolicy) {
		return SandboxStateUnsupported
	}
	if out.State == sandbox.EnforcementDisabled {
		return SandboxStateDisabled
	}
	return SandboxStateFailed
}

func classifyLegacySandboxError(err error) SandboxRunState {
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "not found") || strings.Contains(text, "unsupported") || strings.Contains(text, "not implemented") {
		return SandboxStateUnsupported
	}
	return SandboxStateFailed
}

func (o SandboxOutcome) Configured() SandboxOutcome {
	if o.State == SandboxStateDisabled || o.State == SandboxStateUnsupported || o.State == SandboxStateFailed {
		return o
	}
	o.State = SandboxStateConfigured
	return o
}

func (o SandboxOutcome) Launched() SandboxOutcome {
	if o.State == SandboxStateConfigured {
		o.State = SandboxStateLaunched
	}
	return o
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
