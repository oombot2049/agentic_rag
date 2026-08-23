# P8-01 Deterministic Core Unit Suite Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: domain, Agent, budget, Context, idempotency, Turn and knowledge core

| Gate | Command | Result |
|---|---|---|
| Core coverage suite | `go test -cover ./internal/domain ./internal/agent/... ./internal/contextmgr ./internal/idempotency ./internal/turn ./internal/knowledge` | Pass |
| Determinism source gate | `go test -v ./internal/coretest` | Pass |
| Core repeatability | core package list with `-count=10` | Pass |
| Go 1.26 full suite | `go test -count=1 -shuffle=on ./...` | Pass |
| Go 1.27 full suite | `go test -count=1 ./...` | Pass |
| Vet and static analysis | `go vet ./...`; `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate |

Hermetic statement coverage is 80.2% domain, 66.8% Agent (including its
Docker-backed PostgreSQL adapter), 88.2% action, 83.7% Agent budget, 86.1%
Context, 93.8% idempotency, 85.9% Turn and 92.4% knowledge. The acceptance gate
is the critical behavior set, not a single blended percentage.

The Run matrix now includes `WAITING_APPROVAL` and every transition pair. Public
workflow tests cover approval/resume, queued and requested cancellation,
success/selection, Step/Attempt success, failure and unknown outcomes, Turn
completion/failure/cancellation, immutable returned slices, and rejected
terminal mutation atomicity. The knowledge lineage property test now uses seed
42. A repository source gate rejects wall-clock reads, sleeps, global random
functions and `math/rand/v2` in the designated core test suites.
