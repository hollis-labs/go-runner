# go-runner

A thin substrate composing `go-providers` (CLI adapters) and `go-sandbox`
(resolved access policy) into one `Run` entry point: spawn a CLI under optional
OS confinement, parse its structured output through the adapter, and emit raw
observed events to a callback. It deliberately knows nothing about FSM
transitions, broker sessions or plugin lifecycles — translating its event
alphabet into an app's vocabulary is the wrapper's job.

## Start Here

- `README.md`'s "Boundary statement" and "Event alphabet" sections are the
  contract; read both before changing event emission.
- `runner/runner.go` owns `Run`, config validation and the spawn path.
- `runner/event.go` defines the event alphabet; `runner/exit.go` defines the
  structured `ExitError` and its `Cause`.
- `runner/supervisor.go` owns restart, idle-kill and watchdog.
- `runner/limits.go` (with `limits_unix.go` / `limits_windows.go`) owns
  resource limits.
- `internal/stubcli/` is the fake CLI the end-to-end tests spawn.
- `examples/basic` and `examples/env-passthrough` are runnable.

## Commands

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
```

There is no CI workflow in this repo. Some resource-limit tests are
platform-specific and skip off Linux.

## Boundaries

Event ordering is a published guarantee, not an implementation detail: once the
process starts, `process.started` is first and exactly one of
`process.exited` / `process.timeout` is last, with `provider.event`s between.
If `Run` fails before start, it emits nothing at all. Consumers build state
machines on that, so an event added outside the alphabet or emitted out of
order breaks them silently.

`OnEvent` is called synchronously from the spawn goroutine. A slow callback
blocks the stream; that is documented and intentional, so do not add internal
buffering to paper over it.

The sandbox policy is applied before every start **and every restart** —
`TestSandboxPolicyAppliedBeforeEveryStartAndRestart` and
`TestRunResolvedSandboxRealBackendDeniesAccessOnRestart` exist because a
supervisor restart is exactly where confinement gets silently dropped. A
setup failure must prevent the start rather than falling back to unconfined
(`TestSandboxPolicySetupFailurePreventsStart`), and an ambiguous sandbox
configuration is rejected rather than resolved by precedence
(`TestRunRejectsAmbiguousSandboxConfiguration`).

A clean exit returns nil; everything else returns a structured `*ExitError`
carrying `Cause`, which wrappers key off to choose between auto-recovery and
surfacing to the user. Flattening that to a plain error removes their ability
to tell a crash from a cancellation.
