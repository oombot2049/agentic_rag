# P1-05 application bootstrap test report

- Date: 2026-08-23
- Commit under test: working tree before P1-05 commit
- Toolchain: Go 1.27.0 windows/amd64
- Requirement: [0011 application bootstrap](../requirements/0011-application-bootstrap.md)

## 1. Result

P1-05 passes explicit dependency injection, configuration revalidation, logger
composition, lifecycle rollback, concurrency, static-analysis and build gates.
`internal/app` has 97.7% statement coverage; the extended `internal/config`
suite remains at 96.3%.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./...` | Pass |
| Unit + coverage | `go test -cover ./...` | Pass; app 97.7%, config 96.3%, all packages pass |
| Randomized lifecycle | `go test -shuffle=on -count=30 ./internal/app` | Pass |
| Concurrent lifecycle | `go test -run TestConcurrentLifecycleDoesNotDuplicateCalls -count=100 ./internal/app` | Pass; 2,000 start + 2,000 shutdown callers |
| Static analysis | `go vet ./...` | Pass |
| Build | `go build -buildvcs=false -trimpath -o bin/agenticrag.exe ./cmd/agenticrag` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Lifecycle matrix

| Scenario | Expected | Evidence |
|---|---|---|
| Valid config and factory | Typed Config, logger and BuildInfo injected | Dependency test passes |
| Injected invalid Config | Build fails before factory invocation | Validation test passes |
| Warn log level | Info filtered; static service/environment/version emitted | Logger policy test passes |
| Three components | Start declaration order, stop reverse order | Exact event-order assertion passes |
| Third start fails | First two roll back reverse order | Rollback test passes |
| Start and rollback both fail | Joined error matches both causes | `errors.Is` assertions pass |
| Multiple stop failures | Every component attempted; all causes preserved | Shutdown test passes |
| Run context already canceled | Start then Stop with fresh non-canceled cleanup context | Run test passes |
| Repeated Start/Shutdown | Start rejected; Shutdown idempotent | State test passes |
| 20 simultaneous Start calls | Exactly one component start | 100 repeated runs pass |
| 20 simultaneous Shutdown calls | Exactly one component stop | 100 repeated runs pass |
| Nil/failing/empty/duplicate factory output | Build fails with deterministic location/name | Input table passes |

## 4. Architecture review notes

- No global mutable Config, logger, client or component registry was added.
- Factories receive an explicit immutable-by-convention Dependencies value.
- The package imports no DI/reflection framework.
- Factory construction is separated from resource activation.
- Lifecycle errors wrap causes, and shutdown uses a fresh bounded context.
- The executable still has no server command; later integration owns signals and
  process exit so P1-05 remains hermetic.

## 5. Race detector limitation

The host lacks the C compiler required by Windows `-race`. Mutex-serialized
lifecycle tests ran under heavy repetition, but P1-08 must enforce the actual
race detector on Linux.

## 6. Acceptance conclusion

- No global singleton and every dependency replaceable: **Pass**.
- Deterministic lifecycle and failure rollback: **Pass**.
- Startup/config/logger tests and quality gates: **Pass**.

P1-05 is complete.
