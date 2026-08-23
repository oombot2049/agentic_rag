# P6-05 Tool Audit Test Report

- Date: 2026-08-23
- Result: Pass with explicitly documented local environment skips
- Scope: `internal/audit`, `internal/repository`, migration 000014

| Gate | Command | Result |
|---|---|---|
| Domain/service tests | `go test -cover ./internal/audit` | Pass; 91.5% statement coverage |
| Go 1.26 suite | `go test ./...`; `go vet ./...` | Pass |
| Go 1.27 suite | `go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt -l`; `git diff --check` | Pass |
| PostgreSQL transaction test | `TestToolAuditRepositoryPersistsQueriesAndVerifiesArtifact` | Compiled and checked in; skipped locally because rootless Docker is unsupported on this Windows provider |
| Race detector | `go test -race ./internal/audit` | Not runnable locally because CGO is unavailable; mandatory Ubuntu CI runs `go test -race -count=1 ./...` |

Deterministic tests cover closed enums, machine-safe caller/impact/failure
tokens, approval shape, exact temporal bounds, contiguous non-overlapping
Attempts, outcome agreement, Artifact all-or-none evidence, defensive copies,
tenant non-enumeration, duplicate IDs/Tool Calls and stable Run pagination.

The Testcontainers test applies the real migrations and verifies exact ledger
binding, Artifact tenant/hash/size/media/expiry checks, query by Tool Call and
Run, ordered Attempt hydration, foreign-tenant not-found semantics, and atomic
rollback. The local report does not claim a Docker-backed or race-detector pass.

