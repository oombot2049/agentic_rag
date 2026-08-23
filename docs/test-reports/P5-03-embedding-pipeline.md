# P5-03 Embedding Pipeline Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/embedding`

| Gate | Command | Result |
|---|---|---|
| Fake Provider integration | `go test -cover ./internal/embedding` | Pass; 84.0% statement coverage |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Workflow lint | `actionlint -no-color` | Pass |
| Patch hygiene | `git diff --check` | Pass |

Tests verify configurable logical batches, partial cache hits, tenant/model
version key isolation, limiter admission, ordered output, stable retry request
keys, checkpoint resume and idempotent completion replay. Corrupt provider
counts, dimensions and NaN values are rejected before any cache, cost or
checkpoint commit. Usage assertions cover both tokens and integer micro-cost.
