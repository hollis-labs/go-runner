// Package runner is a thin substrate that composes go-providers (CLI adapters
// + spawn bridges) and go-sandbox (Profile + Apply) into a single Run entry
// point. It spawns a CLI binary under a sandbox profile, parses its structured
// output through a provider adapter, and emits raw observed events through a
// caller-supplied callback.
//
// The runner is intentionally opinion-free about the meaning of those events:
// it does not know what an FSM transition is, what a broker session event is,
// or what a plugin lifecycle is. Apps translate the raw event stream to their
// own vocabulary in their own wrapper layer. See the README for illustrative
// wrapper sketches (Clockwork / Mux / Nanite).
//
// Lifecycle of a Run call:
//
//  1. Resolve binary via cfg.Provider.Detect() (or fail).
//  2. Build *exec.Cmd with cfg.Args, cfg.Env, cfg.Workspace as the working
//     directory. cfg.Stderr (if non-nil) is wired to cmd.Stderr.
//  3. If cfg.Profile is non-zero, wrap the cmd via go-sandbox Apply (mutates
//     cmd.Path/Args; returns cleanup that runs after Wait).
//  4. Set cmd.Cancel = SIGTERM and cmd.WaitDelay (grace-period). The wait-
//     delay is sourced from go-providers' WithWaitDelay context value; the
//     runner installs cfg.WaitDelay onto the context before spawn when set.
//  5. Spawn; emit EventProcessStarted.
//  6. Stream stdout line-by-line through cfg.Provider.ParseLine. Each parsed
//     provider.StreamEvent is emitted as an EventProviderEvent whose Payload
//     carries the StreamEvent verbatim plus an is_turn_complete convenience
//     flag (provider.IsTurnComplete).
//  7. cmd.Wait. Emit EventProcessTimeout when the context's deadline expired,
//     otherwise EventProcessExited (clean or non-zero).
//
// The OnEvent callback is invoked synchronously from the run goroutine. Slow
// callbacks block the stream; consumers should fan out to their own channel
// or goroutine if buffering is required.
package runner
