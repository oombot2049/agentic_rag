# Requirement P1-05: Explicit dependency injection and application lifecycle

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-02 configuration](0008-typed-configuration.md), [P1-03 logging](0009-structured-logging.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-05

## 1. Objective

Create the application composition boundary without a dependency-injection
framework or mutable global registry. Startup must be deterministic, every
dependency replaceable in tests, and partial startup safely reversible.

## 2. Composition flow

```text
ConfigLoader
    │ validated Config
    v
context-aware JSON Logger + BuildInfo
    │ Dependencies value
    v
Factory[0..n] ──construct inactive components──> App
                                                  │
                         Start in declaration order
                         Stop in reverse order
```

`Build` is the composition phase. It loads and revalidates configuration,
constructs the logger, then invokes component factories in declaration order.
Factories receive only the explicit `Dependencies` value. A factory MUST create
an inactive component; sockets, pools, workers and goroutines begin in `Start`,
so a later factory failure does not leak active resources.

## 3. Core contracts

```go
type Component interface {
    Name() string
    Start(context.Context) error
    Stop(context.Context) error
}

type Factory func(Dependencies) (Component, error)
```

Dependencies currently contain typed Config, `*slog.Logger` and normalized
BuildInfo. PostgreSQL, Redis, HTTP and workers join through future factories;
they do not become globals. Components have unique non-empty stable names used
only in internal lifecycle errors and logs.

`Options` allows tests to replace the config loader, log writer, build metadata
and all factories. Defaults are process environment, stdout and embedded build
metadata.

## 4. Lifecycle state machine

```text
ready --Start success--> running --Shutdown--> stopped
  │                         │
  └--Start failure---------┘ (rollback, stopped)
```

- `Start` may succeed only once.
- Components start in declaration order.
- After a start failure, every successfully started component stops in reverse
  order using the configured shutdown timeout; the start and rollback errors are
  joined so `errors.Is/As` preserve both causes.
- `Shutdown` is idempotent, marks the app stopped before external calls and
  attempts every stop even after errors.
- `Run` starts, waits for cancellation, then creates a fresh background shutdown
  context. Cancellation of the run context therefore cannot cancel cleanup.
- Shutdown before Start is a safe no-op and permanently stops that App instance.

Lifecycle methods serialize state transitions with a mutex. Components own the
concurrency safety of their internal Start/Stop operations; the App never starts
or stops the same component twice.

## 5. Logger construction

The config log-level enum maps explicitly to `slog` levels. The logger receives
static `service=agenticrag`, environment and version fields and retains P1-03
context correlation/redaction. Config secrets are never included. Invalid
configuration fails before any factory runs.

## 6. Failure ownership

- Config load/validation: returned directly with safe typed validation details.
- Factory failure: wrapped with its index; no component was started.
- Component start/stop failure: wrapped with its stable component name.
- Process exit codes, OS signals and final log flushing belong to the executable
  integration task; this package remains testable without `os.Exit` or signals.

## 7. Test strategy

Tests use fake components and factories to verify dependency delivery, logger
level/static fields, startup order, reverse shutdown, rollback, joined failure
identity, duplicate/empty/nil components, failed/revalidated config, idempotent
shutdown, repeated start rejection and Run cancellation. No network service is
needed.

## 8. Acceptance criteria

- [x] Dependencies are constructor/factory-injected; no mutable global singleton exists.
- [x] Config is validated before component factories run.
- [x] Components start in order and stop/rollback in reverse order.
- [x] Shutdown attempts all components and preserves multiple errors.
- [x] All dependencies and lifecycle failures are replaceable/testable.
- [x] No reflection-based DI framework or hidden service locator is introduced.

## 9. Follow-up ownership

P1-06/P1-07 add database/cache factories. P3 adds the HTTP component. P4 adds
workers. The executable integration will call `Build`/`Run`, handle signals and
map terminal lifecycle failures to process exit behavior.
