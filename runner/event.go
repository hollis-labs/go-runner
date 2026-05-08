package runner

import "time"

// EventKind identifies the kind of a runner Event. The runner emits a small,
// process-lifecycle-level alphabet; per-event-stream provider events are
// nested under EventProviderEvent with the original provider.StreamEvent in
// Payload.
type EventKind string

const (
	// EventProcessStarted fires once after the CLI has been spawned. Payload:
	//   - "pid"    int     — the OS pid of the spawned process
	//   - "binary" string  — the resolved CLI binary path passed to exec
	//   - "args"   []string — the full argv (including any sandbox/limits wrapper)
	EventProcessStarted EventKind = "process.started"

	// EventProviderEvent carries one parsed provider.StreamEvent. Payload:
	//   - "event"            provider.StreamEvent — the raw event
	//   - "is_turn_complete" bool                 — provider.IsTurnComplete(event)
	EventProviderEvent EventKind = "provider.event"

	// EventProcessExited fires once after cmd.Wait returns and signals the
	// terminal lifecycle moment for the process. Payload:
	//   - "exit_code" int    — process exit status (0 on clean exit; -1 if
	//                          terminated by a signal before exit)
	//   - "signal"    int    — signal number that terminated the process; 0
	//                          for normal exit. Added in v0.3.0.
	//   - "killed"    bool   — true if termination was forced via SIGKILL.
	//                          Added in v0.3.0.
	//   - "cause"     string — one of the Cause* constants when go-runner
	//                          triggered termination, else "". Added in
	//                          v0.3.0.
	//   - "error"     string — empty on clean exit; otherwise wait-error text
	EventProcessExited EventKind = "process.exited"

	// EventProcessTimeout fires in place of EventProcessExited when the
	// run context's deadline expired before the process exited cleanly.
	// Payload:
	//   - "error" string — the context error text (typically
	//                      "context deadline exceeded")
	EventProcessTimeout EventKind = "process.timeout"

	// EventRestart fires before each restart attempt when the supervisor
	// re-spawns after a non-zero exit. Payload:
	//   - "attempt"     int            — 1-indexed restart attempt number
	//   - "prev_exit"   *ExitError     — structured exit info from the previous attempt (nil if unavailable)
	//   - "backoff_for" time.Duration  — the wait before this restart fires
	EventRestart EventKind = "supervisor.restart"

	// EventIdleKill fires when the supervisor's idle-kill triggers (no
	// stdin write or stdout/stderr read for IdleKill). The runner has
	// already SIGTERM'd (with WaitDelay grace) → SIGKILL'd the process by
	// the time this event is emitted. Payload:
	//   - "idle_for" time.Duration — how long the process was idle before kill
	EventIdleKill EventKind = "supervisor.idle_kill"

	// EventWatchdog fires when the supervisor's watchdog triggers (no
	// ActivityCallback tick within WatchdogTimeout, or no stdout/stderr
	// activity in fallback mode). The process has been SIGKILL'd by the
	// time this event is emitted. Payload:
	//   - "no_activity_for" time.Duration — how long since the last activity tick
	EventWatchdog EventKind = "supervisor.watchdog"

	// EventResourceLimitHit fires when a resource limit causes process
	// termination, when the runner can determine which resource. On
	// platforms / paths where the resource cannot be inferred reliably
	// (e.g. RLIMIT_AS overshoot manifesting as ENOMEM-on-malloc), this
	// event may not fire even though ResourceLimits were configured.
	// Payload:
	//   - "resource" string — one of "cpu_time" | "memory" | "open_files" | "processes" | "file_size"
	EventResourceLimitHit EventKind = "supervisor.resource_limit"
)

// Event is one observation of the spawned process or its event stream.
// Payload is keyed by stable strings documented per EventKind.
type Event struct {
	Kind    EventKind
	At      time.Time
	Payload map[string]any
}
