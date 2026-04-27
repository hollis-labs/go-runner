# Changelog

All notable changes to `go-runner` are documented in this file. Per-release
notes are also published as GitHub Releases.

## v0.1.0 — 2026-04-27

Initial release. Thin substrate that composes
[`go-providers`](https://github.com/hollis-labs/go-providers) (CLI adapters +
grace-period spawner) and
[`go-sandbox`](https://github.com/hollis-labs/go-sandbox) (Profile + Apply)
into a single `Run` entry point. Emits raw process-lifecycle and
provider-stream events via callback; apps translate to their own vocabulary
in their own wrapper layer (no Hollis Labs opinions in the lib).

### Public API

- `runner.Config` — `Provider` (`provider.CLIAdapter`), `Profile`
  (`sandbox.Profile`; zero-value skips sandboxing), `Workspace`, `Args`,
  `Env`, `WaitDelay`, `OnEvent`
- `runner.Event` / `runner.EventKind` — four kinds: `process.started`,
  `provider.event`, `process.exited`, `process.timeout`. Payload is
  `map[string]any` with stable per-Kind documented keys.
- `runner.Run(ctx, cfg) error` — orchestrates spawn / sandbox / parse /
  grace-period / cleanup. Returns the wait error (nil on clean exit) plus
  any sandbox-apply or pipe-setup error.

### Composition

- **Spawn / grace-period.** Builds an `*exec.Cmd`, sets
  `cmd.Cancel = SIGTERM` and `cmd.WaitDelay = provider.WaitDelayFromContext(ctx)`,
  then spawns. `cfg.WaitDelay` (when non-zero) is installed onto the context
  via `provider.WithWaitDelay`. Grace-period mechanics live in
  `go-providers v0.5.0`.
- **Sandbox.** When `cfg.Profile.ID` is non-empty, calls
  `sandbox.Apply(cmd, cfg.Profile, cfg.Workspace)` before `cmd.Start`,
  wrapping with `sandbox-exec` (darwin) or `bwrap` (linux). Cleanup runs
  after `cmd.Wait`. Zero-value profile skips sandboxing.
- **Parsing.** Each line read from stdout is passed to
  `cfg.Provider.ParseLine`. Each returned `StreamEvent` is wrapped in a
  `provider.event` runner Event with a `is_turn_complete` convenience flag.

### Order guarantee

Once the process has successfully started, `process.started` is always first
and exactly one of `process.exited` / `process.timeout` is always last.
`provider.event`s appear between them, including any terminal
`EventDone`/`EventError` produced by the adapter. Setup/validation failures
return before any Events are emitted (missing required Config fields,
provider Detect failure, stdout-pipe error, sandbox.Apply error,
`cmd.Start` error).

### Boundary

The lib does not know what an FSM transition is, what a broker session
event is, or what a plugin lifecycle is. Three illustrative wrapper sketches
ship in the README (Clockwork FSM transition / Mux broker event / Nanite
plugin lifecycle) — none in the package.

### Dependencies

- `github.com/hollis-labs/go-providers` v0.5.0
- `github.com/hollis-labs/go-sandbox` v0.1.0

### Verification

- darwin host: `go build ./...`, `go vet ./...`, `go test -race -timeout 60s ./...` — 5 PASS (~1.7s)
- linux cross-compile: `GOOS=linux go build ./...`, `go vet ./...` — ok
- Live linux run with `bwrap` installed is a known follow-up.

### Origin

Clockwork ticket `CW-20260427-0023` under epic `EP-20260426-0002` (CLI
substrate libraries — go-providers / go-sandbox / go-runner). Initial PR
[#1](https://github.com/hollis-labs/go-runner/pull/1).
