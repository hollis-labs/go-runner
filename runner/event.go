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
	//   - "args"   []string — the full argv (including any sandbox wrapper)
	EventProcessStarted EventKind = "process.started"

	// EventProviderEvent carries one parsed provider.StreamEvent. Payload:
	//   - "event"            provider.StreamEvent — the raw event
	//   - "is_turn_complete" bool                 — provider.IsTurnComplete(event)
	EventProviderEvent EventKind = "provider.event"

	// EventProcessExited fires once after cmd.Wait returns and signals the
	// terminal lifecycle moment for the process. Payload:
	//   - "exit_code" int    — process exit status (0 on clean exit; -1 if
	//                          the wait error did not surface an *exec.ExitError)
	//   - "error"     string — empty on clean exit; otherwise wait-error text
	EventProcessExited EventKind = "process.exited"

	// EventProcessTimeout fires in place of EventProcessExited when the
	// run context's deadline expired before the process exited cleanly.
	// Payload:
	//   - "error" string — the context error text (typically
	//                      "context deadline exceeded")
	EventProcessTimeout EventKind = "process.timeout"
)

// Event is one observation of the spawned process or its event stream.
// Payload is keyed by stable strings documented per EventKind.
type Event struct {
	Kind    EventKind
	At      time.Time
	Payload map[string]any
}
