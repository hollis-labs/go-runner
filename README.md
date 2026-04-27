# go-runner

Thin Go substrate that composes [`go-providers`](https://github.com/hollis-labs/go-providers)
(CLI adapters + spawn helpers) and [`go-sandbox`](https://github.com/hollis-labs/go-sandbox)
(Profile + Apply) into a single `Run` entry point. It spawns a CLI binary
under a sandbox profile, parses its structured output through a provider
adapter, and emits **raw observed events** through a caller-supplied
callback.

```go
import "github.com/hollis-labs/go-runner/runner"

err := runner.Run(ctx, runner.Config{
    Provider:  myAdapter,                    // provider.CLIAdapter
    Profile:   myProfile,                    // sandbox.Profile (zero = no sandbox)
    Workspace: "/abs/path/to/workspace",
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

If you find yourself wanting to add an app-specific helper here, that's a
sign the helper belongs in your wrapper layer instead.

## Event alphabet

The runner emits four `EventKind` values:

| Kind                         | When                                                    | Payload keys                               |
| ---------------------------- | ------------------------------------------------------- | ------------------------------------------ |
| `process.started`            | once, after `cmd.Start` returns                         | `pid`, `binary`, `args`                    |
| `provider.event`             | per parsed `provider.StreamEvent` from stdout           | `event` (raw `provider.StreamEvent`), `is_turn_complete` |
| `process.exited`             | once, after `cmd.Wait` returns (clean or non-zero)      | `exit_code`, `error`                       |
| `process.timeout`            | once, in place of `process.exited` when ctx deadline hit | `error`                                    |

Order guarantee: `process.started` is always first, exactly one of
`process.exited`/`process.timeout` is always last. `provider.event`s appear
between them, including any terminal `EventDone`/`EventError` produced by
the adapter (`is_turn_complete=true` flags those for consumers).

`OnEvent` is invoked **synchronously** from the spawn goroutine. Slow
callbacks block the stream; fan out to your own channel or goroutine if
buffering is needed.

## Composition

- **Spawn / grace-period.** `runner.Run` builds an `*exec.Cmd`, sets
  `cmd.Cancel = SIGTERM` and `cmd.WaitDelay = provider.WaitDelayFromContext(ctx)`,
  and spawns. `cfg.WaitDelay` (when non-zero) is installed onto the
  context via `provider.WithWaitDelay`. The grace-period mechanics
  themselves live in [`go-providers`](https://github.com/hollis-labs/go-providers).
- **Sandbox.** When `cfg.Profile.ID` is non-empty, the runner calls
  `sandbox.Apply(cmd, cfg.Profile, cfg.Workspace)` before `cmd.Start`,
  wrapping with `sandbox-exec` (darwin) or `bwrap` (linux). The cleanup
  closure runs after `cmd.Wait`. A zero-value profile skips sandboxing
  entirely.
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
- Raw event emission via callback
- Process-lifecycle terminal-event guarantee (always exactly one of
  `process.exited` / `process.timeout` per Run)

## Out of scope

- App vocabulary (FSM transitions, broker events, plugin lifecycle).
- Stderr capture or aggregation. Stderr passes through whatever the
  caller configured before `Run` (and by default to the inherited
  process's stderr).
- Output formats other than line-delimited (newline-terminated). The
  underlying scanner uses a 1 MiB max line size to match `go-providers`.
- Adapter argument construction. `Args` is the raw argv. If you want
  adapter-aware arg building, call `cfg.Provider.BuildArgs(...)` in your
  wrapper before populating `Config.Args`.
- Process-group signaling. `cmd.Cancel` only signals the direct child;
  CLIs that fork long-running children inherit stdout and may delay exit.
  See `go-providers` follow-up notes.

## License

MIT — see [LICENSE](./LICENSE).
