# P6-06 Built-in Tool Examples Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/tools/builtin`

| Gate | Command | Result |
|---|---|---|
| Contract/end-to-end tests | `go test -cover ./internal/tools/builtin` | Pass; 92.0% statement coverage |
| Repeatability | `go test -count=10 ./internal/tools/builtin` | Pass |
| Go 1.26 suite | `go test ./...`; `go vet ./...` | Pass |
| Go 1.27 suite | `go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt -l`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate; local Windows environment has no CGO race runtime |

Tests execute knowledge search and ticket creation through a sealed Registry
and the bounded Executor. Ticket creation also traverses the Tool Ledger. They
prove exact tenant/session/Run propagation, explicit permissions/effects,
closed schemas, no-network capabilities, stable invocation-key propagation,
idempotent adapter retry, terminal ledger replay and exactly one downstream
creation.

Negative cases cover unknown fields, whitespace/control constraints, invalid
priority, malformed invocation key, wrong adapter identity, backend failure,
invalid IDs and results exceeding `top_k`. Backend error details are replaced
with the package-level error and are not exposed to the Agent.

