# P1-03 structured logging test report

- Date: 2026-08-23
- Commit under test: working tree before P1-03 commit
- Toolchain: Go 1.27.0 windows/amd64
- Requirement: [0009 structured logging](../requirements/0009-structured-logging.md)

## 1. Result

P1-03 passes its context-correlation, anti-spoofing, sensitive-data, concurrency,
static-analysis and build gates. `internal/observability/log` has 97.5% statement
coverage.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./...` | Pass |
| Unit + coverage | `go test -cover ./...` | Pass; log package 97.5%, all packages pass |
| Randomized repetition | `go test -shuffle=on -count=30 ./internal/observability/log` | Pass |
| Concurrency repetition | `go test -run TestConcurrentContextsDoNotLeak -count=50 ./internal/observability/log` | Pass; 5,000 request records checked |
| Static analysis | `go vet ./...` | Pass |
| Full regression | `go test ./...` | Pass |
| Build | `go build -buildvcs=false -trimpath -o bin/agenticrag.exe ./cmd/agenticrag` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Behavior matrix

| Scenario | Expected | Evidence |
|---|---|---|
| Ingress + domain context | Six available IDs appear automatically | JSON field test passes |
| Incremental scope | Tenant/request/trace preserved; Session/Turn/Run added | Merge test passes |
| Missing context | Empty correlation fields omitted | Omission test passes |
| Caller spoofs `tenant_id` | Caller values dropped; trusted context wins once | Anti-spoof test passes |
| Prompt/retrieval/credential attrs | Values replaced by `[REDACTED]` | Top-level and nested tests pass |
| Logger-scoped API key | Redacted before storage in child handler | `With` test passes |
| Prompt hash/count metadata | Remains queryable | Safe-key tests pass |
| Info below warn threshold | Record not emitted | Level test passes |
| 100 simultaneous contexts | Request ID matches its own ordinal exactly | 50 repeated runs pass |

## 4. Leak-resistance notes

Test fixtures deliberately send marker values through Prompt, retrieval,
password and API-key attributes and assert that no marker occurs in raw JSON.
The test also asserts only one `tenant_id` key is serialized after an attempted
record- and logger-level spoof. Message bodies remain a caller policy boundary;
the design therefore requires static event names and forbids content in `msg`.

## 5. Race detector limitation

The Windows host lacks the C compiler required by Go's race detector. Normal
concurrent tests and high repetition pass, but they are not represented as a
race-detector substitute. P1-08 must enforce `go test -race ./...` on Linux.

## 6. Acceptance conclusion

- Automatic tenant/request/trace/session/turn/run correlation: **Pass**.
- Reserved-field integrity and secret/content redaction: **Pass**.
- Concurrent request isolation: **Pass**.
- Repository quality gates: **Pass**.

P1-03 is complete.
