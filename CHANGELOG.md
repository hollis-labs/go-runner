# Changelog

All notable changes to `go-runner` are documented in this file. Per-release
notes are also published as GitHub Releases.

## v0.7.0 — 2026-09-06

- Add `Config.SandboxPolicy` for resolved access policies and enforce the
  policy before every initial spawn and supervised restart.
- Prevent process start when required sandbox setup fails; retain legacy
  profile compatibility and explicit enforcement outcomes.
- Update shared dependencies to go-providers v0.26.0 and go-sandbox v0.3.0.

## v0.6.0 — 2026-05-26

Dependency bump. No exported `runner` API changes; existing consumers can
upgrade transparently. Unblocks the `agentkit` module merge by aligning
`go-runner` with the same `go-providers` / `go-llm-types` / `go-sandbox`
versions `agentkit` already pins.

### Dependency bumps

- `github.com/hollis-labs/go-providers` v0.12.0 → v0.23.0
- `github.com/hollis-labs/go-llm-types` v0.1.0 → v0.3.0
- `github.com/hollis-labs/go-llm-contracts` v0.1.0 → v0.3.0 (indirect)
- `github.com/hollis-labs/go-sandbox` v0.1.0 → v0.2.1

### Public API

No exported `runner` symbols changed signature. The `CLIAdapter` interface
(`Detect`, `Name`, `ParseLine`) is unchanged between go-providers v0.12.0
and v0.23.0. `llmtypes.IsTurnComplete`, `provider.WithWaitDelay`,
`provider.WaitDelayFromContext`, and `provider.DefaultWaitDelay` are all
unchanged. No source edits in `runner/` were required.

### Verification

- darwin host: `go mod tidy` (no-op), `go vet ./...`, `go build ./...`,
  `go test -race -count=1 -timeout 180s ./...` — green.

## v0.5.0 — 2026-05-10

Public-release prep. No exported `runner` API changes; existing
consumers can upgrade transparently.

### Added

- `examples/basic` — runnable demo wiring the Claude CLI adapter
  (`provider.NewClaudeAdapter`) into `runner.Run` and pretty-printing
  every event. Run via `go run ./examples/basic -prompt "..."`.
- `examples/env-passthrough` — demonstrates `Config.Env` semantics
  (nil inherit, empty slice, explicit slice) via a minimal inline
  `CLIAdapter` over `/usr/bin/env`. Doubles as a template for adapting
  non-LLM line-delimited CLIs.

### Documentation

- README gains an Examples section linking the two new programs.
- CHANGELOG narrative scrubbed for public consumption (internal
  workspace paths and ticket IDs removed; technical content
  preserved).

### Verification

- darwin host: `gofmt -l .` clean; `go vet ./...`, `go build ./...`,
  `go test -race -count=1 -timeout 180s ./...` — green.
- `go mod tidy` is a no-op (modules already minimal).

## v0.4.0 — 2026-05-09

Combines the supervision / resource-limits / ExitError surface
(originally written under the v0.3.0 entry but never shipped — the
v0.3.0 tag was cut on `main` before the feature branch landed) with
`go-providers` v0.12.0 compatibility.

### go-providers v0.12.0 compat (consumer-side migration)

- Bumped `github.com/hollis-labs/go-providers` from v0.5.0 to v0.12.0.
- Added `github.com/hollis-labs/go-llm-types` v0.1.0.
- Migrated removed-alias references in `runner/`:
  `provider.IsTurnComplete` → `llmtypes.IsTurnComplete`,
  `provider.StreamEvent` → `llmtypes.StreamEvent`,
  `provider.EventDelta` / `EventDone` → `llmtypes.EventDelta` / `EventDone`.
- `provider.CLIAdapter`, `provider.WithWaitDelay`, and
  `provider.WaitDelayFromContext` continue to live in `go-providers`
  (CLI/PTY/subprocess surface) and are unchanged.

### Public API

No exported `runner` symbols changed signature in the migration. The
runner's `Config.Provider` field remains `provider.CLIAdapter`; the
adapter interface itself now returns `[]llmtypes.StreamEvent` from
`ParseLine` (per `go-providers` v0.12.0). Consumers that pass adapters
constructed from `go-providers` (the production case) get this
transparently. Consumers that implement their own `CLIAdapter` need to
update their `ParseLine` return type to `[]llmtypes.StreamEvent`.

### Verification

- darwin host: `go vet ./...`, `go build ./...`,
  `go test -race -count=1 ./...` — green.

## v0.3.0 — 2026-05-08

Adds structured exit info, opt-in process supervision, and OS-native
resource limits. Pulls process-lifetime policy (idle-kill, watchdog,
restart, OS-level rlimits) into the lib so apps don't each roll their
own; app vocabulary (FSM transitions, broker events, plugin lifecycle)
still lives in wrappers.

### Public API additions

- `runner.ExitError` (returned via `errors.As`) — `Code`, `Signal`,
  `Killed`, `ProcessState`, `Cause`. Wraps the underlying wait error
  via `Unwrap`. Existing callers that only check `err != nil` are
  unchanged. Clean exits return `nil`.
- `runner.Cause*` constants: `CauseIdleTimeout`, `CauseWatchdogKill`,
  `CauseRestartExhausted`, `CauseOOMKill`, `CauseResourceLimit`.
- `runner.Config.Supervisor *SupervisorOptions` — opt-in process
  supervision (idle-kill, restart-on-crash, watchdog).
  `SupervisorOptions` fields: `IdleKill`, `IdleKillGrace`,
  `RestartOnCrash`, `MaxRestartBackoff`, `WatchdogTimeout`,
  `ActivityCallback`. Default `nil` preserves prior behavior.
- `runner.Config.ResourceLimits ResourceLimits` — opt-in OS resource
  caps. Fields: `CPUTime`, `MemoryMax`, `MaxOpenFiles`, `MaxProcesses`,
  `MaxFileSize`. Zero value = unlimited.
- New event kinds: `EventRestart`, `EventIdleKill`, `EventWatchdog`,
  `EventResourceLimitHit`. See README event alphabet for stable
  payload shapes.

### Behavior changes

- `Run` now returns `*ExitError` (extractable via `errors.As`) for
  non-clean exits. The function signature itself is unchanged
  (`func Run(ctx, cfg) error`).
- `EventProcessExited` payload gains stable keys: `signal`, `killed`,
  `cause`, alongside existing `exit_code` and `error`. Removing
  existing keys would be a break; adding new ones is additive per
  the README's stable-shape contract.
- Supervisor goroutines are spawned only when `cfg.Supervisor != nil`.
  No goroutines, no timers, no cost when supervision is disabled.

### Per-platform resource limits

- **Linux + systemd-run --user available**: `MemoryMax` enforced via
  cgroup v2 (real OOM-kill). Other limits via `ulimit`.
- **Linux without systemd**: all limits via `ulimit` setrlimit (memory
  via RLIMIT_AS — advisory; document as such).
- **macOS**: all limits via `ulimit` except `MemoryMax`, which is
  silently dropped (RLIMIT_AS not exposed via darwin's bash; no
  systemd; VM isolation is the right answer for darwin and out of
  scope here).
- **Windows**: unsupported; `applyResourceLimitsImpl` returns an
  error if a non-zero `ResourceLimits` is configured.

### Known gaps / caveats

- Go's runtime swallows `SIGXCPU` on at least darwin, so Go-binary
  consumers may not terminate at the `CPUTime` soft limit. Native
  C-based binaries (sh, claude, codex, etc.) honor `SIGXCPU`
  normally. Documented in the Resource limits README section.
- `bash`'s `ulimit -f` defaults to 1024-byte blocks (not POSIX 512);
  go-runner's wrap matches that default. No effect on linux's dash.

### Verification

- darwin host: `go vet ./...`, `go test -race -timeout 180s ./...` —
  green (16 tests including new ExitError, supervision, and
  resource-limits coverage).
- linux cross-compile: `GOOS=linux go build ./...`,
  `GOOS=linux go vet ./...` — ok.
- Live linux run with systemd-run + bwrap is a known follow-up;
  test `TestResourceLimits_MemoryMax_Linux` skips when systemd-run
  --user is unavailable.

## v0.2.0 — 2026-04-27

Adds caller-controlled stderr capture so wrapper-driven executors can
fan stderr out to per-run sidecar logs without the runner taking an
opinion on aggregation or routing.

### Public API additions

- `runner.Config.Stderr io.Writer` — when non-nil, wired to `cmd.Stderr`
  before spawn. Bytes flow through verbatim; the runner does not parse,
  buffer, or aggregate stderr. Pass `io.MultiWriter` to fan out (e.g.
  in-memory tail buffer plus a file). Nil leaves `cmd.Stderr` unset,
  which `os/exec` routes to `os.DevNull` (no behavior change for v0.1
  callers).

### Other

- README "In scope" / "Out of scope" sections updated to reflect Stderr
  passthrough.
- `internal/stubcli` gains a `-stderr-msg <line>` flag for testing the
  Stderr passthrough path.
- New tests: `TestRun_Stderr_CapturesToWriter`,
  `TestRun_Stderr_NilLeavesCmdStderrUnset`.

### Verification

- darwin host: `go build ./...`, `go vet ./...`, `go test -race -timeout 60s ./...` — 7 PASS
- linux cross-compile: `GOOS=linux go build ./...`, `go vet ./...` — ok

## v0.1.0 — 2026-04-27

Initial release. Thin substrate that composes
[`go-providers`](https://github.com/hollis-labs/go-providers) (CLI adapters +
grace-period spawner) and
[`go-sandbox`](https://github.com/hollis-labs/go-sandbox) (Profile + Apply)
into a single `Run` entry point. Emits raw process-lifecycle and
provider-stream events via callback; apps translate to their own vocabulary
in their own wrapper layer — no app-specific opinions in the lib.

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

Initial PR: [#1](https://github.com/hollis-labs/go-runner/pull/1).
