# P3-08 Run Cancellation — Test Report

- Date: 2026-08-23
- Scope: idempotent cancel API, PostgreSQL control transition, worker safe point, event/outbox atomicity
- Result: PASS for all executable local gates; mandatory Docker/race execution is checked into Linux CI

## Acceptance evidence

| Invariant | Evidence |
|---|---|
| Same-key idempotency | PostgreSQL integration launches 50 concurrent cancel calls and requires identical `CANCEL_REQUESTED` responses with one state/event mutation. Unit tests cover in-progress, replay/reuse and invalid keys. |
| Running two-phase cancel | Integration requires `RUNNING -> CANCEL_REQUESTED`, retained lease, Session bookkeeping, then safe-point `CANCELLED`, lease release, Turn cancellation and Session release. |
| Queued immediate cancel | Integration requires `QUEUED -> CANCELLED`, two ordered public events and atomic Session/Turn release. |
| Next-Step race | `TestCancellationAndNextStepStartAreExecutionVersionFenced` races cancel against Step insertion; only “Step committed first” or `ErrLeaseFenced` is legal. |
| Side-effect history | A completed TOOL Step remains `SUCCEEDED` after cancellation acknowledgement; cancellation performs no Step/Attempt/Artifact rollback. |
| Stale worker | Released or mismatched lease authority maps to `RUN_FENCED`; the repository also rejects an expired lease timestamp. |
| Atomic rollback | An injected Outbox unique-key failure after control/event work must leave Run `RUNNING`, execution version 1, no public event and no idempotency claim. |
| Tenant isolation | Foreign tenant command maps to the same `RUN_NOT_FOUND` identity as absence; HTTP ignores untrusted tenant headers. |
| HTTP boundary | Tests require `sessions:write`, one idempotency header, empty body, no query, trusted tenant, 202 response and stable Location. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint                                                  PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/run coverage                                       67.1%
internal/transport/http coverage                            90.6%
```

The local Windows host cannot run the remaining platform gates:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/repository ...
  rootless Docker is not supported on Windows

go test -race ./internal/run ./internal/transport/http
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

These failures are not reported as passes. `.github/workflows/ci.yml` mandates
Docker-backed integration and `go test -race -count=1 ./...` on Ubuntu.
