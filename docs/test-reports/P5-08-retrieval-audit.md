# P5-08 Retrieval Audit Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/retrieval`, `internal/repository`, migration 000011

| Gate | Command | Result |
|---|---|---|
| Service/repository/schema tests | `go test -cover ./internal/retrieval ./internal/repository ./migrations` | Pass; retrieval 86.3%, repository 41.2% (repository contains broad integration adapters) |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Workflow/patch lint | `actionlint -no-color`; `git diff --check` | Pass |
| PostgreSQL round trip/rollback | `TestRetrievalAuditRepositoryRoundTripTenantIsolationAndSnapshotFailure` | Checked in; skipped locally when Docker provider is unavailable |

Deterministic tests cover IDs, contiguous ordinals, unique Chunks, finite scores,
selection shape, bounded tenant retention and defensive score copies. The
Testcontainers case verifies real migration/FKs, all candidate fields,
tenant-scoped non-enumeration and zero partial rows for a mismatched Artifact
hash. Local verification does not claim a Docker-backed pass.
