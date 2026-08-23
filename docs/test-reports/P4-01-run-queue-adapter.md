# P4-01 RunQueue Adapter — Test Report

- Date: 2026-08-23
- Scope: vendor-neutral queue port, PostgreSQL visibility leases, Outbox adapter, trace propagation and capacity
- Result: PASS for all executable local gates; mandatory Docker/race execution is checked into Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Replaceable port | Compile-time assertion proves `PostgresAdapter` implements vendor-free `RunQueue`; application messages contain only domain IDs, JSON, time and W3C trace metadata. |
| Outbox handoff | `RunQueuedAdapter` strict-decodes `RunQueued.v1`, checks tenant/Run identity and propagates the outbox delivery ID. Forged, unknown or already-published events are rejected. |
| Trace continuity | Create Turn validates single-value canonical `traceparent/tracestate`; trace is excluded from the business fingerprint, persisted in Outbox and preserved in claimed Delivery. |
| Partition ordering | PostgreSQL integration enqueues two Runs in one tenant+Session and requires only the older message to claim until it is Acked. |
| At-least-once | Nack delay, expired-lease reclaim and delivery counters are tested; tokens advance 1→2→3 and stale token 2 cannot Ack token 3. |
| Fencing | Unit and integration tests cover Ack/Nack/Renew owner/token/expiry classification as `ErrFenced` or `ErrLeaseExpired`. |
| Capacity | 20 concurrent same-tenant enqueues under cap 10 produce exactly 10 durable messages and 10 tenant-capacity failures; a second tenant fills the global cap and the next enqueue receives global-capacity failure. |
| Tenant isolation | Claim and cleanup always require tenant; foreign tenant cannot observe another tenant's message. |
| Lifecycle | Acked rows are removed only by bounded tenant+cutoff cleanup. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint                                                  PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/queue unit coverage                                78.8%
queue contract randomized repetition (100x)                 PASS
```

The local Windows host cannot execute the remaining platform gates:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/queue -run TestPostgresRunQueue
  rootless Docker is not supported on Windows

go test -race ./internal/queue ./internal/turn ./internal/transport/http
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

These are not reported as local passes. `.github/workflows/ci.yml` mandates
Docker-backed integration and `go test -race -count=1 ./...` on Ubuntu.
