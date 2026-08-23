# P7-01 OpenTelemetry Tracing Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: HTTP, Run queue/Worker, PostgreSQL, Redis, RAG, Rerank, LLM and `internal/observability/trace`

| Gate | Command | Result |
|---|---|---|
| SDK export integration | `go test -cover ./internal/observability/trace` | Pass; 91.0% statement coverage |
| Repeatability | `go test -count=10 ./internal/observability/trace` | Pass |
| Go 1.26 suite | `go test ./...`; `go vet ./...` | Pass |
| Go 1.27 suite | `go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass; OTel API/SDK aligned at v1.41.0 |
| Formatting/patch hygiene | repository-scoped `gofmt -l`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate; local Windows environment has no CGO race runtime |

The exporter test starts from a sampled remote W3C parent, executes HTTP, RAG,
LLM, PostgreSQL and Redis instrumentation, persists the current HTTP span
carrier, then starts a new asynchronous Run trace and a Run-child RAG span. It
asserts different request/Run Trace IDs, no Run parent, exactly one Link to the
HTTP span, and correct child parenting.

Sensitive sentinels are placed in URL path/query, SQL/table/tenant predicate,
Redis keys and raw backend errors. The test inspects every exported span
attribute and event and proves none are exported. SQL verbs, Redis commands,
RAG stages and HTTP methods are closed allowlists; error events contain only a
normalized class. Turn-handler tests additionally prove the current SERVER
span—not the remote header—is what enters durable queue metadata.

