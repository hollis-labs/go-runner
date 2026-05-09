package runner

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SupervisorOptions configures opt-in process supervision: idle-kill,
// restart-on-crash, and watchdog. The zero value (Config.Supervisor ==
// nil) preserves go-runner's default "spawn once, run to completion,
// return" behavior.
//
// Supervision applies to a single Run call. Restart re-spawns a fresh
// subprocess with the same Provider / Args / Env / Workspace / Profile
// / ResourceLimits; supervision does not migrate across processes.
type SupervisorOptions struct {
	// IdleKill terminates the process when no I/O activity (stdout line
	// observed or stderr byte written) occurs for this duration. The
	// runner first sends SIGTERM, waits IdleKillGrace, then SIGKILL if
	// the process is still alive. Zero disables idle-kill.
	IdleKill time.Duration

	// IdleKillGrace is the SIGTERM→SIGKILL grace period for idle-kill.
	// Zero defaults to 5s.
	IdleKillGrace time.Duration

	// RestartOnCrash sets the maximum number of restart attempts after a
	// non-zero exit. Zero disables restart (single shot). The runner
	// performs initial run + up to RestartOnCrash restarts; total
	// process spawns is RestartOnCrash + 1.
	//
	// Backoff between attempts is exponential (1s, 2s, 4s, ...) capped
	// at MaxRestartBackoff. The context is honored during backoff:
	// cancellation aborts further restarts.
	RestartOnCrash int

	// MaxRestartBackoff caps the exponential restart backoff. Zero
	// defaults to 30s.
	MaxRestartBackoff time.Duration

	// WatchdogTimeout fires SIGKILL with Cause=watchdog_kill if no
	// ActivityCallback invocations or stdout/stderr activity occur
	// within this duration. Zero disables the watchdog.
	WatchdogTimeout time.Duration

	// ActivityCallback is populated by the runner when supervision
	// starts. Callers should read this field after Run begins (e.g.
	// from inside OnEvent — by the time OnEvent fires, ActivityCallback
	// has been set) and invoke it whenever the caller observes
	// "meaningful" activity from the child. Useful when the caller
	// wants a stricter notion of activity than raw stdout/stderr I/O.
	//
	// The watchdog also resets on stdout-line / stderr-write events
	// observed by the runner; ActivityCallback supplements that signal,
	// it does not replace it.
	//
	// Set by the runner; ignored if non-nil at Run entry (the runner
	// always overwrites with its own thread-safe trampoline).
	ActivityCallback func()
}

// supState carries supervisor-driven termination cause across goroutines
// so the post-Wait error can be classified.
type supState struct {
	mu    sync.Mutex
	cause string
}

func (s *supState) trySetCause(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause != "" {
		return false
	}
	s.cause = c
	return true
}

func (s *supState) getCause() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause
}

// activityTracker is a lock-free last-activity timestamp store shared
// between the stdout scanner, the stderr tap, and the supervision
// goroutines. Time is stored as unix nanoseconds in an atomic int64
// (zero = never ticked).
type activityTracker struct {
	last atomic.Int64
}

func (a *activityTracker) tick() {
	a.last.Store(time.Now().UnixNano())
}

// idleSince returns time.Since(lastTick), or 0 if never ticked. Callers
// should treat 0 as "no activity yet" (idle from start).
func (a *activityTracker) idleSince(start time.Time) time.Duration {
	last := a.last.Load()
	if last == 0 {
		return time.Since(start)
	}
	return time.Since(time.Unix(0, last))
}

// activityTap wraps an io.Writer so each Write also ticks the tracker.
// Used to observe stderr activity even though go-runner does not
// interpret stderr bytes.
type activityTap struct {
	w io.Writer
	a *activityTracker
}

func (t *activityTap) Write(p []byte) (int, error) {
	if t.w == nil {
		// No caller stderr writer configured; still tick activity but
		// drop the bytes (matches the os/exec default of routing nil
		// Stderr to os.DevNull).
		t.a.tick()
		return len(p), nil
	}
	n, err := t.w.Write(p)
	if n > 0 {
		t.a.tick()
	}
	return n, err
}

// installActivityTap returns an io.Writer that taps writes for activity
// tracking. If origStderr is nil the tap still consumes (and counts)
// stderr bytes; the runner will assign the result to cmd.Stderr.
func installActivityTap(origStderr io.Writer, a *activityTracker) io.Writer {
	return &activityTap{w: origStderr, a: a}
}

// superviseIdle polls the activity tracker; if idle exceeds threshold,
// it sets the cause, emits EventIdleKill, sends SIGTERM, then SIGKILL
// after the grace period.
func superviseIdle(
	cfg Config,
	cmd *exec.Cmd,
	activity *activityTracker,
	state *supState,
	startedAt time.Time,
	procDone <-chan struct{},
) {
	threshold := cfg.Supervisor.IdleKill
	grace := cfg.Supervisor.IdleKillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	tick := threshold / 4
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	timer := time.NewTicker(tick)
	defer timer.Stop()

	for {
		select {
		case <-procDone:
			return
		case <-timer.C:
			idle := activity.idleSince(startedAt)
			if idle < threshold {
				continue
			}
			if !state.trySetCause(CauseIdleTimeout) {
				return // another supervisor already triggered
			}
			cfg.OnEvent(Event{
				Kind:    EventIdleKill,
				At:      time.Now(),
				Payload: map[string]any{"idle_for": idle},
			})
			killWithGrace(cmd, grace, procDone)
			return
		}
	}
}

// superviseWatchdog polls the activity tracker; if the gap since last
// activity exceeds WatchdogTimeout, it SIGKILLs the process directly
// (no SIGTERM grace, per spec).
func superviseWatchdog(
	cfg Config,
	cmd *exec.Cmd,
	activity *activityTracker,
	state *supState,
	startedAt time.Time,
	procDone <-chan struct{},
) {
	threshold := cfg.Supervisor.WatchdogTimeout
	tick := threshold / 4
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	timer := time.NewTicker(tick)
	defer timer.Stop()

	for {
		select {
		case <-procDone:
			return
		case <-timer.C:
			idle := activity.idleSince(startedAt)
			if idle < threshold {
				continue
			}
			if !state.trySetCause(CauseWatchdogKill) {
				return
			}
			cfg.OnEvent(Event{
				Kind:    EventWatchdog,
				At:      time.Now(),
				Payload: map[string]any{"no_activity_for": idle},
			})
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
			return
		}
	}
}

// killWithGrace sends SIGTERM, waits grace, then SIGKILL if the process
// is still alive. Returns when procDone is signalled (process exited)
// or the grace + kill sequence completes.
func killWithGrace(cmd *exec.Cmd, grace time.Duration, procDone <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-procDone:
		return
	case <-time.After(grace):
	}
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGKILL)
	}
}

// computeRestartBackoff returns the wait duration before restart attempt
// `attempt` (1-indexed). Doubles per attempt, capped at maxBackoff.
// Defaults to 30s cap if maxBackoff is non-positive.
func computeRestartBackoff(attempt int, maxBackoff time.Duration) time.Duration {
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 30 {
		return maxBackoff
	}
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	if base > maxBackoff {
		return maxBackoff
	}
	return base
}

// runSupervised wraps runOnce in a restart loop driven by
// cfg.Supervisor.RestartOnCrash. The first attempt always runs; up to
// RestartOnCrash additional attempts follow on non-zero exit. Backoff
// honours the parent context.
func runSupervised(ctx context.Context, cfg Config) error {
	maxAttempts := cfg.Supervisor.RestartOnCrash + 1
	var (
		lastErr error
		lastXE  *ExitError
	)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := computeRestartBackoff(attempt-1, cfg.Supervisor.MaxRestartBackoff)
			cfg.OnEvent(Event{
				Kind: EventRestart,
				At:   time.Now(),
				Payload: map[string]any{
					"attempt":     attempt - 1,
					"prev_exit":   lastXE,
					"backoff_for": backoff,
				},
			})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		err := runOnce(ctx, cfg)
		if err == nil {
			return nil
		}
		lastErr = err
		lastXE = nil
		var xe *ExitError
		if errors.As(err, &xe) {
			lastXE = xe
		}
		if ctx.Err() != nil {
			return err
		}
	}
	if lastXE != nil && lastXE.Cause == "" {
		lastXE.Cause = CauseRestartExhausted
	}
	return lastErr
}
