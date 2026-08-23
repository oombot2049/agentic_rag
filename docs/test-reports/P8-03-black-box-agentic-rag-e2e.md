# P8-03 Black-box AgenticRAG E2E Test Report

- Result: Pass with local Docker scenario skipped; mandatory Linux CI gate added
- Date: 2026-08-23
- Go versions: 1.26.7 and 1.27.0
- Container: `postgres:18.6-alpine`

## Scope

The tagged suite drives only authenticated HTTP and SSE client interfaces. It
creates a Session and Turn, verifies idempotent replay, rejects a changed-body
idempotency-key reuse and a context-version conflict, relays the transactional
Outbox record into the production PostgreSQL RunQueue, and lets the production
Worker execute the Run.

The Worker processor composes the production Run lease manager, PostgreSQL Agent
checkpoint store, Agent Machine, Tool Registry/Executor, built-in
`knowledge.search` adapter, durable event repository, and atomic final commit.
The model decision and knowledge result are deterministic fakes at their normal
ports. The client disconnects after its first SSE event and resumes strictly
after that sequence using `Last-Event-ID`.

Public GET APIs prove a selected terminal Run, one user plus one assistant
message, the expected document/chunk citation, Session context advancement, and
release of `active_run_id`. A final durable assertion proves exactly one Turn,
one Run, one succeeded Tool Step, one knowledge call, and an acknowledged Queue
delivery.

## Commands and results

| Gate | Go 1.26.7 | Go 1.27.0 |
| --- | --- | --- |
| `go vet ./...` | Pass | Pass |
| `go vet -tags=e2e ./tests/e2e` | Pass | Pass |
| `go test -count=1 ./...` | Pass | Pass |
| `go test -run '^$' -tags=e2e ./tests/e2e` | Pass | Pass |
| reproducible command build | Pass | Pass |
| `staticcheck ./...` and tagged E2E | Pass | Not required |

`go test -tags=e2e -count=1 -v ./tests/e2e` also passed its Docker-free
contract test locally. The container scenario skipped because rootless Docker
is unsupported on this Windows host. CI exports `AGENTICRAG_REQUIRE_DOCKER=1`,
so the same condition is a hard failure on the Linux release runner and the full
scenario cannot silently skip there.

The real-provider smoke compiled and skipped because no provider credentials
were supplied, as designed.

## Diagnostics

Every PostgreSQL container is registered for cleanup. On a scenario failure,
the suite captures at most 256 KiB of container logs before cleanup. Client
requests, SSE replay, Worker settlement, and the overall test each have bounded
timeouts; secrets and model response bodies are not emitted.
