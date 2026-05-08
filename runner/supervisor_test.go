package runner_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/go-runner/runner"
)

func TestSupervisor_IdleKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix signals only")
	}
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var (
		mu             sync.Mutex
		sawIdleKill    bool
		exitedPayload  map[string]any
	)

	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-sleep", "10s"},
		WaitDelay: 2 * time.Second,
		Supervisor: &runner.SupervisorOptions{
			IdleKill:      400 * time.Millisecond,
			IdleKillGrace: 200 * time.Millisecond,
		},
		OnEvent: func(ev runner.Event) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Kind {
			case runner.EventIdleKill:
				sawIdleKill = true
				if d, ok := ev.Payload["idle_for"].(time.Duration); !ok || d < 400*time.Millisecond {
					t.Errorf("EventIdleKill idle_for = %v, want >=400ms", ev.Payload["idle_for"])
				}
			case runner.EventProcessExited:
				exitedPayload = ev.Payload
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runner.Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected non-nil error after idle-kill")
	}
	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	if xe.Cause != runner.CauseIdleTimeout {
		t.Errorf("ExitError.Cause = %q, want %q", xe.Cause, runner.CauseIdleTimeout)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawIdleKill {
		t.Error("EventIdleKill not observed")
	}
	if exitedPayload == nil {
		t.Fatal("EventProcessExited not observed")
	}
	if got, _ := exitedPayload["cause"].(string); got != runner.CauseIdleTimeout {
		t.Errorf("EventProcessExited cause = %q, want %q", got, runner.CauseIdleTimeout)
	}
}

func TestSupervisor_Watchdog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix signals only")
	}
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	// stubcli emits 1 delta + done, then sleeps 10s. The watchdog timer
	// observes no activity past the initial output and SIGKILLs the
	// process.
	var (
		mu          sync.Mutex
		sawWatchdog bool
	)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-sleep", "10s"},
		WaitDelay: 2 * time.Second,
		Supervisor: &runner.SupervisorOptions{
			WatchdogTimeout: 400 * time.Millisecond,
		},
		OnEvent: func(ev runner.Event) {
			if ev.Kind == runner.EventWatchdog {
				mu.Lock()
				defer mu.Unlock()
				sawWatchdog = true
				if d, ok := ev.Payload["no_activity_for"].(time.Duration); !ok || d < 400*time.Millisecond {
					t.Errorf("EventWatchdog no_activity_for = %v, want >=400ms", ev.Payload["no_activity_for"])
				}
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runner.Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected non-nil error after watchdog kill")
	}
	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	if xe.Cause != runner.CauseWatchdogKill {
		t.Errorf("ExitError.Cause = %q, want %q", xe.Cause, runner.CauseWatchdogKill)
	}
	if xe.Signal != int(syscall.SIGKILL) {
		t.Errorf("ExitError.Signal = %d, want %d (SIGKILL)", xe.Signal, syscall.SIGKILL)
	}
	if !xe.Killed {
		t.Error("ExitError.Killed = false, want true")
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawWatchdog {
		t.Error("EventWatchdog not observed")
	}
}

func TestSupervisor_RestartOnCrash_ExhaustsAttempts(t *testing.T) {
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var (
		mu                sync.Mutex
		startedCount      int
		exitedCount       int
		restartCount      int
		lastExitedPayload map[string]any
	)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1", "-fail"},
		Supervisor: &runner.SupervisorOptions{
			RestartOnCrash:    2,
			MaxRestartBackoff: 100 * time.Millisecond,
		},
		OnEvent: func(ev runner.Event) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Kind {
			case runner.EventProcessStarted:
				startedCount++
			case runner.EventProcessExited:
				exitedCount++
				lastExitedPayload = ev.Payload
			case runner.EventRestart:
				restartCount++
				if att, _ := ev.Payload["attempt"].(int); att != restartCount {
					t.Errorf("EventRestart attempt = %v, want %d", ev.Payload["attempt"], restartCount)
				}
			}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := runner.Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected non-nil error after restart exhaustion")
	}
	var xe *runner.ExitError
	if !errors.As(err, &xe) {
		t.Fatalf("error is not *ExitError: %T %v", err, err)
	}
	if xe.Code != 2 {
		t.Errorf("ExitError.Code = %d, want 2 (-fail exits 2)", xe.Code)
	}
	if xe.Cause != runner.CauseRestartExhausted {
		t.Errorf("ExitError.Cause = %q, want %q", xe.Cause, runner.CauseRestartExhausted)
	}

	mu.Lock()
	defer mu.Unlock()
	if startedCount != 3 {
		t.Errorf("EventProcessStarted count = %d, want 3 (initial + 2 restarts)", startedCount)
	}
	if exitedCount != 3 {
		t.Errorf("EventProcessExited count = %d, want 3", exitedCount)
	}
	if restartCount != 2 {
		t.Errorf("EventRestart count = %d, want 2", restartCount)
	}
	if got, _ := lastExitedPayload["exit_code"].(int); got != 2 {
		t.Errorf("last EventProcessExited exit_code = %v, want 2", lastExitedPayload["exit_code"])
	}
}

func TestSupervisor_RestartOnCrash_RecoversBeforeExhaustion(t *testing.T) {
	// Stubcli always exits 0 (no -fail); RestartOnCrash should be a no-op
	// because the first attempt succeeds. Verifies the success path.
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	var (
		mu           sync.Mutex
		startedCount int
		restartCount int
	)
	cfg := runner.Config{
		Provider:  &stubAdapter{binPath: bin},
		Workspace: workspace,
		Args:      []string{"-count", "1"},
		Supervisor: &runner.SupervisorOptions{
			RestartOnCrash:    3,
			MaxRestartBackoff: 100 * time.Millisecond,
		},
		OnEvent: func(ev runner.Event) {
			mu.Lock()
			defer mu.Unlock()
			switch ev.Kind {
			case runner.EventProcessStarted:
				startedCount++
			case runner.EventRestart:
				restartCount++
			}
		},
	}

	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("clean exit returned error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if startedCount != 1 {
		t.Errorf("EventProcessStarted count = %d, want 1 (no restart on clean exit)", startedCount)
	}
	if restartCount != 0 {
		t.Errorf("EventRestart count = %d, want 0", restartCount)
	}
}

func TestSupervisor_ActivityCallback_PopulatedAtRunStart(t *testing.T) {
	// The runner overwrites SupervisorOptions.ActivityCallback at Run
	// entry. Caller can read the populated callback from inside OnEvent
	// (Start happens-before the first OnEvent).
	bin := buildStubCLI(t)
	workspace := t.TempDir()

	supOpts := &runner.SupervisorOptions{
		WatchdogTimeout: 1 * time.Second,
	}
	var sawCallback bool
	cfg := runner.Config{
		Provider:   &stubAdapter{binPath: bin},
		Workspace:  workspace,
		Args:       []string{"-count", "1"},
		Supervisor: supOpts,
		OnEvent: func(ev runner.Event) {
			if ev.Kind == runner.EventProcessStarted && supOpts.ActivityCallback != nil {
				sawCallback = true
				supOpts.ActivityCallback() // smoke test that it doesn't panic
			}
		},
	}

	if err := runner.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawCallback {
		t.Error("ActivityCallback was not populated by the time EventProcessStarted fired")
	}
}
