# P1-06 PostgreSQL foundation test report

- Date: 2026-08-23
- Toolchain: Go 1.27.0 windows/amd64
- Dependencies: pgx/v5.10.0, Testcontainers Go v0.43.0
- Requirement: [0012 PostgreSQL foundation](../requirements/0012-postgresql-foundation.md)

## Result

All hermetic parser, configuration, DSN non-disclosure, regression, vet and
build gates pass. The real PostgreSQL Testcontainers suite compiles and was
invoked, but correctly reported **SKIP** because this Windows host has no Docker
provider. It is not claimed as a local integration pass; P1-08 must run the same
test with Docker mandatory on Linux.

| Gate | Result |
|---|---|
| `go test -v -cover ./internal/platform/postgres` | Pass with one explicit Docker skip; hermetic coverage 28.7% |
| `go test -shuffle=on -count=50 -run ... ./internal/platform/postgres` | Pass |
| `go test ./...` | Pass |
| `go vet ./...` | Pass |
| reproducible executable build | Pass |
| `git diff --check` | Pass |
| Testcontainers PostgreSQL 18.6 execution | Not executed: Docker unavailable |

The invoked integration suite verifies, when Docker is available: bounded Open
and Health, exact pool max-open setting, migration up idempotence, checksum
drift rejection, callback-error rollback, successful commit, panic rollback and
reverse migration. Container cleanup is registered immediately.

Hermetic tests verify strict filename parsing, order, up/down pairing, gap and
name-conflict rejection, stable SHA-256, nil constructor inputs, nil transaction
work and that an invalid DSN containing marker credentials cannot appear in the
returned error string. `go.sum` locks all downloaded content.

## Acceptance

- Pool/health timeout implementation and tests: **Pass (hermetic/config); real DB pending CI**.
- Transaction and migration rollback implementation: **Pass (compiled); real DB pending CI**.
- Migration parser/validation: **Pass locally**.
- Secret non-disclosure, vet, regression and build: **Pass locally**.

P1-06 implementation is complete; its environment-dependent verification gate
is explicitly delegated to the mandatory Docker job in P1-08.
