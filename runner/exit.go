package runner

import (
	"fmt"
	"os"
)

// Cause* describe the high-level reason a process terminated when the
// runner triggered the termination directly. Empty Cause indicates a
// normal-but-non-zero exit or signal not driven by go-runner.
const (
	CauseIdleTimeout      = "idle_timeout"
	CauseWatchdogKill     = "watchdog_kill"
	CauseRestartExhausted = "restart_exhausted"
	CauseOOMKill          = "oom_kill"
	CauseResourceLimit    = "resource_limit"
)

// ExitError is the structured outcome of a non-clean process exit. Run
// returns an *ExitError (wrapping the underlying wait error) for any
// non-zero or signal-terminated exit; callers extract the structured info
// via errors.As. Clean exits (Code == 0) return nil.
type ExitError struct {
	// Code is the process exit code, or -1 if the process was terminated
	// by a signal before exit.
	Code int

	// Signal is the signal number that terminated the process, or 0 if
	// the process exited normally (cleanly or via non-zero status).
	Signal int

	// Killed is true when the termination was forced via SIGKILL (idle-
	// kill, watchdog-kill, OOM-kill, or other forced kill).
	Killed bool

	// ProcessState is the raw os.ProcessState for callers that need
	// platform-specific details beyond the structured fields.
	ProcessState *os.ProcessState

	// Cause classifies the termination when go-runner triggered it
	// directly. One of the Cause* constants, or empty string for
	// normal-but-non-zero exits not driven by supervisor / limits.
	Cause string

	waitErr error
}

func (e *ExitError) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Cause != "":
		return fmt.Sprintf("runner: process terminated (cause=%s, signal=%d, code=%d)", e.Cause, e.Signal, e.Code)
	case e.Signal != 0:
		return fmt.Sprintf("runner: process terminated by signal %d", e.Signal)
	default:
		return fmt.Sprintf("runner: process exited %d", e.Code)
	}
}

// Unwrap returns the underlying wait error (typically *exec.ExitError or
// a context error) so errors.Is / errors.As keep working against the
// stdlib types alongside *ExitError.
func (e *ExitError) Unwrap() error { return e.waitErr }
