package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

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
	Stderr io.Writer

	// WaitDelay is the grace period between SIGTERM (on context cancel)
	// and SIGKILL. Zero falls through to provider.DefaultWaitDelay.
	WaitDelay time.Duration

	// OnEvent receives each runner Event synchronously from the spawn
	// goroutine. Required.
	OnEvent func(Event)
}

// Run spawns cfg.Provider's binary under cfg.Profile, streams its stdout
// through the adapter, and emits runner Events via cfg.OnEvent until the
// process exits or the context is cancelled. Run returns the wait error
// (nil on clean exit) plus any sandbox-apply or pipe-setup error. Setup
// and validation failures (missing required Config fields, provider
// Detect failure, stdout-pipe error, sandbox.Apply error, cmd.Start
// error) return before any Events are emitted; once the process has
// successfully started, the terminal Event (process.exited or
// process.timeout) is always emitted before Run returns.
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
	if cfg.Stderr != nil {
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

	cfg.OnEvent(Event{
		Kind: EventProcessStarted,
		At:   time.Now(),
		Payload: map[string]any{
			"pid":    cmd.Process.Pid,
			"binary": cmd.Path,
			"args":   append([]string(nil), cmd.Args...),
		},
	})

	streamProviderEvents(stdout, cfg)

	waitErr := cmd.Wait()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		cfg.OnEvent(Event{
			Kind:    EventProcessTimeout,
			At:      time.Now(),
			Payload: map[string]any{"error": ctx.Err().Error()},
		})
	default:
		exitCode := -1
		errText := ""
		if waitErr != nil {
			errText = waitErr.Error()
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				exitCode = ee.ExitCode()
			}
		} else {
			exitCode = 0
		}
		cfg.OnEvent(Event{
			Kind: EventProcessExited,
			At:   time.Now(),
			Payload: map[string]any{
				"exit_code": exitCode,
				"error":     errText,
			},
		})
	}

	return waitErr
}

// streamProviderEvents reads stdout line-by-line, runs each line through the
// adapter, and emits one EventProviderEvent per parsed StreamEvent. Parse
// errors are silently dropped to match go-providers' bridge behavior; the
// adapter is the authority on what counts as a parseable line.
func streamProviderEvents(stdout io.ReadCloser, cfg Config) {
	defer stdout.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
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
					"is_turn_complete": provider.IsTurnComplete(ev),
				},
			})
		}
	}
}
