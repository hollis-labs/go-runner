# go-runner

Thin Go substrate that composes [`go-providers`](https://github.com/hollis-labs/go-providers)
(CLI adapters + spawn helpers) and [`go-sandbox`](https://github.com/hollis-labs/go-sandbox)
(resolved access policy + legacy Profile enforcement) into a single `Run` entry point. It spawns a CLI binary under optional OS confinement, parses its structured output through a provider adapter, and emits **raw observed events** through a caller-supplied callback.

```go
import "github.com/hollis-labs/go-runner/runner"

resolved, _ := sandbox.ResolveAccessPolicy(policy)

err := runner.Run(ctx, runner.Config{
    Provider:      myAdapter,                // provider.CLIAdapter
    SandboxPolicy: &resolved,                // preferred: required/disabled resolved policy
    Workspace:     "/abs/path/to/workspace",
    Args:      []string{"--prompt", "hi"},
    Env:       nil,                          // nil = inherit parent env
    WaitDelay: 5 * time.Second,              // SIGTERM → SIGKILL grace; 0 = lib default
    OnEvent: func(ev runner.Event) {
        // translate ev.Kind / ev.Payload to your app's vocabulary
    },
})
```

## Boundary statement

**This library does not know what an FSM transition is.**

It does not know what a broker session event is. It does not know what a
plugin lifecycle is. It emits a small alphabet of process-lifecycle and
provider-stream events; it is your wrapper's job to translate those
observations into your app's vocabulary.

That boundary is deliberate. The same spawn-and-parse machinery powers
durable executors (Clockwork), session brokers (Mux), and plugin hosts
(Nanite). Each app translates the raw event stream differently. Putting
those translations into the library would couple the library to one
opinion; putting them in per-app wrappers keeps the library generic and
the app's vocabulary close to its own state machine.

**What changed in v0.3.0:** the lib now also owns *process-lifetime
policy* — supervision (idle-kill, restart-on-crash, watchdog) and OS
resource limits. Those are still process-level, not app-level: they
describe how the process lives and dies, not what the process events
mean. Termination cause is exposed as a small enum (`Cause*` constants
on `ExitError`); apps decide what to do with `idle_timeout` /
`watchdog_kill` / `oom_kill` / `restart_exhausted` in their own
wrappers (e.g. nanite's recovery-broker pattern keys off `Cause` to
choose between auto-recover and surface-to-user).

If you find yourself wanting to add an app-specific helper here, that's a
sign the helper belongs in your wrapper layer instead.

## Event alphabet

The runner emits the following `EventKind` values:

| Kind                          | When                                                          | Payload keys                                                                  |
| ----------------------------- | ------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `process.started`             | once, after `cmd.Start` returns                               | `pid`, `binary`, `args`, `sandbox` (`SandboxOutcome`)                         |
| `provider.event`              | per parsed `provider.StreamEvent` from stdout                 | `event` (raw `provider.StreamEvent`), `is_turn_complete`                      |
| `process.exited`              | once, after `cmd.Wait` returns (clean or non-zero)            | `exit_code`, `signal`, `killed`, `cause`, `error`, `sandbox`                  |
| `process.timeout`             | once, in place of `process.exited` when ctx deadline hit      | `error`, `sandbox`                                                            |
| `supervisor.restart`          | before each restart attempt (when `Supervisor.RestartOnCrash` is configured) | `attempt` (1-indexed), `prev_exit` (`*ExitError`), `backoff_for` (`time.Duration`) |
| `supervisor.idle_kill`        | when `Supervisor.IdleKill` triggers                           | `idle_for` (`time.Duration`)                                                  |
| `supervisor.watchdog`         | when `Supervisor.WatchdogTimeout` triggers                    | `no_activity_for` (`time.Duration`)                                           |
| `supervisor.resource_limit`   | when a `ResourceLimits` field causes termination (best-effort) | `resource` (`"cpu_time" \| "memory" \| "open_files" \| "processes" \| "file_size"`) |

Order guarantee: once the process has successfully started, `process.started`
is always first and exactly one of `process.exited`/`process.timeout` is
always last. `provider.event`s appear between them, including any terminal
`EventDone`/`EventError` produced by the adapter (`is_turn_complete=true`
flags those for consumers). If `Run` returns an error before spawning or
starting the process (config validation, provider Detect failure, stdout
pipe, sandbox Apply, or `cmd.Start` failure), no events are emitted.

`OnEvent` is invoked **synchronously** from the spawn goroutine. Slow
callbacks block the stream; fan out to your own channel or goroutine if
buffering is needed.

### Structured `ExitError`

For non-clean exits, `Run` returns an `*ExitError` (extractable via
`errors.As`) carrying:

```go
type ExitError struct {
    Code         int             // process exit code; -1 if signal-terminated
    Signal       int             // signal number that terminated the process; 0 for clean exit
    Killed       bool            // true iff terminated by SIGKILL specifically
    ProcessState *os.ProcessState
    Cause        string          // one of CauseIdleTimeout / CauseWatchdogKill / CauseRestartExhausted / CauseOOMKill / CauseResourceLimit, or "" for ordinary exits
}
```

Existing callers that only check `if err != nil` are unaffected. Clean
exits return `nil`.


## Sandbox policy

Prefer `Config.SandboxPolicy *sandbox.ResolvedAccessPolicy` for new callers. The runner applies it with `sandbox.ApplyResolved` before every `cmd.Start`, including supervisor restarts. Required setup failures return a `*runner.SandboxError` and no process lifecycle events are emitted. If setup succeeds but `cmd.Start` fails, the returned `*runner.StartError` carries a `SandboxOutcome` in the `configured` state so callers do not report launched enforcement.

`SandboxOutcome` is intentionally sanitized: it records policy ID, backend, disabled/unsupported/configured/launched state, unsupported capabilities and diagnostics, but it does not copy argv or environment values. Start, exit and timeout events include the same outcome under `Payload["sandbox"]`; after a successful start the state is `launched`.

Legacy callers may continue to set `Config.Profile`. `Config.Profile` and `Config.SandboxPolicy` are mutually exclusive. Legacy profile outcomes are marked `Legacy=true` and `LegacyDefaultAllow=true` because the old shape is compatibility/default-allow semantics. A zero-value profile and nil resolved policy means explicit unwrapped execution and reports `disabled`.

## Supervision

Opt in via `Config.Supervisor *SupervisorOptions`. The zero value
(`Supervisor: nil`) preserves the default "spawn once, run to completion"
behavior — no goroutines, no timers, no restart loop.

| Option              | Triggers                                                                                       | Termination signal                                | `ExitError.Cause`         |
| ------------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------- | ------------------------- |
| `IdleKill`          | no I/O activity (stdout line / stderr write) for the configured duration                       | `SIGTERM` → `IdleKillGrace` (default 5s) → `SIGKILL` | `idle_timeout`            |
| `RestartOnCrash`    | non-zero exit; up to `RestartOnCrash` retries                                                  | n/a (re-spawn)                                    | `restart_exhausted` (final) |
| `WatchdogTimeout`   | no `ActivityCallback` invocation **or** stdout/stderr activity within the configured duration  | `SIGKILL` directly (no grace)                     | `watchdog_kill`           |

When to use which:

- **`IdleKill`** is for "how long can the process sit completely silent before we assume it's wedged?" Suitable for chat sessions and long-lived agents (recommended 15m).
- **`WatchdogTimeout`** is for "how long can the process go without making *meaningful* progress?" Caller wires `ActivityCallback` from inside `OnEvent` to define what counts as progress (e.g. only `is_turn_complete=true` events). Useful for batch workers and structured pipelines (recommended 30s–2m depending on task).
- **`RestartOnCrash`** is for "the process crashed; try again." Exponential backoff (1s, 2s, 4s, ...) capped at `MaxRestartBackoff` (default 30s). Backoff respects context cancellation.

Example:

```go
cfg := runner.Config{
    /* ... */,
    Supervisor: &runner.SupervisorOptions{
        IdleKill:          15 * time.Minute,
        RestartOnCrash:    3,
        MaxRestartBackoff: 30 * time.Second,
        WatchdogTimeout:   90 * time.Second,
    },
    OnEvent: func(ev runner.Event) {
        if ev.Kind == runner.EventProviderEvent {
            se := ev.Payload["event"].(provider.StreamEvent)
            if se.Type == provider.EventDone && cfg.Supervisor.ActivityCallback != nil {
                cfg.Supervisor.ActivityCallback() // app-defined "progress" signal
            }
        }
    },
}
```

`ActivityCallback` is populated by the runner at `Start` (the field is
non-nil by the time the first `OnEvent` fires). It supplements the
runner's automatic stdout/stderr ticks; if the caller never invokes it,
the watchdog falls back to raw I/O activity.

## Resource limits

Opt in via `Config.ResourceLimits` (zero value = unlimited).

| Field           | Linux (systemd available)              | Linux (no systemd)              | macOS                                                         | Windows |
| --------------- | -------------------------------------- | ------------------------------- | ------------------------------------------------------------- | ------- |
| `CPUTime`       | `ulimit -t` (RLIMIT_CPU)               | `ulimit -t` (RLIMIT_CPU)        | `ulimit -t` (RLIMIT_CPU)                                      | unsupported |
| `MemoryMax`     | systemd-run `MemoryMax` (cgroup v2; real OOM-kill) | `ulimit -v` (RLIMIT_AS; advisory) | **silently dropped** — no VM-isolation; `RLIMIT_AS` not exposed via `ulimit -v` on darwin's bash | unsupported |
| `MaxOpenFiles`  | `ulimit -n`                            | `ulimit -n`                     | `ulimit -n`                                                   | unsupported |
| `MaxProcesses`  | `ulimit -u`                            | `ulimit -u`                     | `ulimit -u`                                                   | unsupported |
| `MaxFileSize`   | `ulimit -f` (1024-byte blocks)         | `ulimit -f`                     | `ulimit -f`                                                   | unsupported |

systemd-run availability is probed once per process via
`systemd-run --user --version`. Probe failure (missing binary,
no user bus, Alpine, minimal containers) cleanly falls back to
ulimit-only enforcement; the runner does not error.

**macOS memory limits.** macOS bash's `ulimit -v` does not bind to
`RLIMIT_AS` (which itself is not exposed there), and there is no
systemd. `MemoryMax` is silently dropped on darwin. Callers needing
hard memory limits on macOS should use VM-based isolation (Lima,
OrbStack) — outside go-runner's scope.

**Layering with `sandbox.Apply`.** When both `Profile` and
`ResourceLimits` are non-zero, the wrap is `[systemd-run]
sh -c "ulimit ..."` outermost, then `sandbox-exec`/`bwrap`, then the
real binary. Limits inherit through every fork-exec.

Note: Go's runtime swallows `SIGXCPU` on at least darwin — Go binaries
configured with `CPUTime` may not terminate at the soft limit. Native
C-based binaries (sh, yes, dd, claude, codex) honor `SIGXCPU` normally.

## Examples

Two runnable examples ship under `examples/`:

- **`examples/basic`** — spawn the Claude CLI under the runner with no
  sandbox and no supervision; print every event to stdout. Requires
  `claude` on `$PATH`.

  ```sh
  go run ./examples/basic -prompt "explain quicksort in one sentence"
  ```

- **`examples/env-passthrough`** — demonstrate `Config.Env` semantics
  (nil = inherit parent env; `[]string{}` = empty env; explicit slice =
  exactly those entries). Spawns `/usr/bin/env` through a minimal
  `CLIAdapter` so you can see what the child process actually sees.

  ```sh
  RUNNER_DEMO_INHERITED=from-parent go run ./examples/env-passthrough -mode inherit
  go run ./examples/env-passthrough -mode empty
  go run ./examples/env-passthrough -mode custom
  ```

  The minimal `echoAdapter` in this example is also a useful template
  for adapting non-LLM line-delimited CLIs without pulling in a full
  provider package.

## Composition

- **Spawn / grace-period.** `runner.Run` builds an `*exec.Cmd`, sets
  `cmd.Cancel = SIGTERM` and `cmd.WaitDelay = provider.WaitDelayFromContext(ctx)`,
  and spawns. `cfg.WaitDelay` (when non-zero) is installed onto the
  context via `provider.WithWaitDelay`. The grace-period mechanics
  themselves live in [`go-providers`](https://github.com/hollis-labs/go-providers).
- **Sandbox.** When `cfg.SandboxPolicy` is non-nil, the runner calls
  `sandbox.ApplyResolved(cmd, *cfg.SandboxPolicy)` before `cmd.Start`. When
  `cfg.Profile.ID` is non-empty, it calls the legacy
  `sandbox.Apply(cmd, cfg.Profile, cfg.Workspace)` adapter instead. The cleanup
  closure runs after `cmd.Wait`. A nil policy plus zero-value profile skips
  sandboxing and records a disabled outcome.
- **Parsing.** Each line read from stdout is passed to
  `cfg.Provider.ParseLine`. Each returned `StreamEvent` is wrapped in a
  `provider.event` runner event. Parse errors are silently dropped to
  match `go-providers` bridge behavior.

## Illustrative wrappers (not in this lib)

These sketches show how three different consumers translate the same raw
event stream into their own vocabulary. They are intentionally NOT
included as packages — each app owns its own wrapper.

### Clockwork — translate to FSM transitions

```go
// In Clockwork's executor plugin:
runner.Run(ctx, runner.Config{
    /* ... */,
    OnEvent: func(ev runner.Event) {
        switch ev.Kind {
        case runner.EventProcessStarted:
            task.Transition(ctx, "doing")
        case runner.EventProviderEvent:
            if ev.Payload["is_turn_complete"].(bool) {
                se := ev.Payload["event"].(provider.StreamEvent)
                if se.Type == provider.EventError {
                    task.Transition(ctx, "blocked", se.Error)
                }
            }
        case runner.EventProcessExited:
            if code, _ := ev.Payload["exit_code"].(int); code == 0 {
                task.Transition(ctx, "review")
            } else {
                task.Transition(ctx, "blocked", ev.Payload["error"].(string))
            }
        case runner.EventProcessTimeout:
            task.Transition(ctx, "blocked", "deadline exceeded")
        }
    },
})
```

### Mux — translate to broker session events

```go
// In Mux's session manager:
runner.Run(ctx, runner.Config{
    /* ... */,
    OnEvent: func(ev runner.Event) {
        switch ev.Kind {
        case runner.EventProcessStarted:
            broker.Publish(sessionID, "session.opened", ev.Payload)
        case runner.EventProviderEvent:
            se := ev.Payload["event"].(provider.StreamEvent)
            broker.Publish(sessionID, "session.delta", map[string]any{
                "type": string(se.Type), "content": se.Content,
            })
        case runner.EventProcessExited, runner.EventProcessTimeout:
            broker.Publish(sessionID, "session.closed", ev.Payload)
        }
    },
})
```

### Nanite — translate to plugin lifecycle

```go
// In Nanite's plugin host:
runner.Run(ctx, runner.Config{
    /* ... */,
    OnEvent: func(ev runner.Event) {
        switch ev.Kind {
        case runner.EventProcessStarted:
            plugin.Lifecycle.Started(ev.Payload["pid"].(int))
        case runner.EventProviderEvent:
            se := ev.Payload["event"].(provider.StreamEvent)
            plugin.Lifecycle.Activity(se)
        case runner.EventProcessExited:
            plugin.Lifecycle.Stopped(ev.Payload["exit_code"].(int))
        case runner.EventProcessTimeout:
            plugin.Lifecycle.TimedOut()
        }
    },
})
```

Notice how each wrapper makes different choices about what counts as a
state transition, what payload to serialize, and how to handle the
terminal events. None of those choices belong in this library.

## In scope

- Spawn + grace-period (delegated to `go-providers`)
- Sandbox wrapping (delegated to `go-sandbox`)
- Line-by-line stdout streaming via `cfg.Provider.ParseLine`
- Stderr passthrough to a caller-supplied `io.Writer` via `Config.Stderr`
  (use `io.MultiWriter` for sidecar logging + in-memory tail)
- Raw event emission via callback
- Process-lifecycle terminal-event guarantee (always exactly one of
  `process.exited` / `process.timeout` per Run)
- Structured `*ExitError` (returned via `errors.As`) carrying
  `Code` / `Signal` / `Killed` / `Cause` / `ProcessState`
- Opt-in process supervision: `IdleKill`, `RestartOnCrash`,
  `WatchdogTimeout`, `ActivityCallback`
- Opt-in OS resource limits: `CPUTime`, `MemoryMax`, `MaxOpenFiles`,
  `MaxProcesses`, `MaxFileSize` (per-platform support; see Resource
  limits section)

## Out of scope

- App vocabulary (FSM transitions, broker events, plugin lifecycle).
- Stderr aggregation or interpretation. The runner only wires
  `cfg.Stderr` to `cmd.Stderr`; bytes flow through verbatim. When
  `cfg.Stderr` is nil, `cmd.Stderr` stays unset and `os/exec` routes to
  `os.DevNull` (its default for nil `Stderr`). Callers wanting parent-
  process passthrough should pass `os.Stderr` explicitly.
- Output formats other than line-delimited (newline-terminated). The
  underlying scanner uses a 1 MiB max line size to match `go-providers`.
- Adapter argument construction. `Args` is the raw argv. If you want
  adapter-aware arg building, call `cfg.Provider.BuildArgs(...)` in your
  wrapper before populating `Config.Args`.
- Process-group signaling. `cmd.Cancel` only signals the direct child;
  CLIs that fork long-running children inherit stdout and may delay exit.
  See `go-providers` follow-up notes.
- Hard memory limits on macOS — out of scope without VM-based isolation
  (Lima / OrbStack). The runner silently drops `MemoryMax` on darwin
  rather than promising a limit it can't enforce.
- Direct cgroups v2 manipulation via `containerd/cgroups`. The current
  Linux memory-limit path shells out to `systemd-run --user --scope`;
  direct cgroups is a follow-up if the shell-out path hits a real
  limitation.
- Per-syscall sandboxing (seccomp). That's `go-sandbox`'s lane.
- Checkpoint / restore (CRIU).

## License

MIT — see [LICENSE](./LICENSE).
