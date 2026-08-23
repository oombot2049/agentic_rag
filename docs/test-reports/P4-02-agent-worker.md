# P4-02 Agent Worker — Test Report

- Date: 2026-08-23
- Scope: queue consumption, tenant/global concurrency, lease renewal, retry settlement and graceful drain
- Result: PASS for all executable local gates; mandatory Docker/race execution is checked into Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Global bound | Twelve deliveries across two tenants block in the Processor; observed and internal peak active counts never exceed four. |
| Tenant bound | The same test caps each tenant at two concurrent tasks and verifies both recorded tenant peaks. |
| Fair polling | PostgreSQL exposes bounded ready tenants; the scheduler rotates tenants and applies stable per-worker ±10% jitter plus capped exponential error backoff. |
| Graceful drain | Stop cancels the scheduler before signaling admitted work. A two-message test proves only one claim occurs, the active Processor observes `DrainRequested`, persists its modeled safe point, and Nacks before Stop returns. |
| Forced stop | A deadline cancels the task context only after the graceful window; the cooperative Processor exits and all Worker-owned goroutines reach zero. |
| Lease safety | Visibility renewal runs during processing. A fenced renewal cancels processing and suppresses stale Ack/Nack so lease expiry owns redelivery. |
| Failure handling | Processor panic is recovered without logging the panic value/payload and becomes a bounded generic retry. Ack and Retry are explicit dispositions. |
| Lifecycle observability | Snapshot exposes lifecycle state, active/peak tenant counts, claims/outcomes/panics/fencing and Worker-owned goroutines; stop-before-start, double-start and repeated stop semantics are tested. |
| Configuration | Worker identity, dual concurrency bounds, poll/lease/renew/retry durations are typed, safe-field allowlisted and cross-field validated. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/worker statement coverage                          86.2%
worker shuffled repetition (100x)                           PASS
```

The local Windows host cannot execute the remaining platform gates:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/queue -run TestPostgresRunQueue
  rootless Docker is not supported on Windows

go test -race ./internal/worker
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

These are not reported as local passes. `.github/workflows/ci.yml` mandates
Docker-backed queue integration and `go test -race -count=1 ./...` on Ubuntu.
