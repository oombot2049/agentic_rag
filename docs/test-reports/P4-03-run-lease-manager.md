# P4-03 Run Lease Manager — Test Report

- Date: 2026-08-23
- Scope: CAS acquisition, heartbeat renewal, serialized execution versions, expiry takeover and Checkpoint recovery
- Result: PASS for all executable local gates; mandatory PostgreSQL/race execution is checked into Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Single winner | Forty simultaneous Managers read one Run version; exactly one acquires and 39 receive lease-unavailable CAS/held classifications. |
| Database authority | The Manager delegates acquire, renew and release to the tenant-scoped PostgreSQL repository; no Redis or in-memory lock grants execution rights. |
| Version serialization | A per-handle context-aware gate serializes heartbeat with short durable mutations. Tests advance acquire version 2 → renew 3 → control mutation 4 → renew 5. |
| Fencing | After expiry takeover advances token 1 → 2, a token-1 Checkpoint write receives repository fencing and permanently cancels the old handle. |
| Checkpoint resume | The new generation receives step 7 and the immutable payload written by token 1 while all future writes use token 2. |
| Lease-loss cancellation | Renewal fencing, local expiry and parent cancellation transition ACTIVE → LOST and cancel the execution context with a queryable cause. |
| Graceful close | Close stops heartbeat, waits behind durable work, releases exact owner/token/version and transitions to CLOSED; repeated calls return the same result. |
| Failure rollback | Unexpected Checkpoint load failure releases a newly acquired lease best-effort and refuses to start execution without recovery state. |
| Real SQL path | Testcontainers expires a real Run lease, acquires token 2, reloads token-1 Checkpoint, and proves the old Worker cannot advance it. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/runlease statement coverage                        78.9%
runlease shuffled repetition (100x)                         PASS
```

The local Windows host cannot execute the remaining platform gates:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/platform/postgres -run TestRunLeaseManagerPostgres
  rootless Docker is not supported on Windows

go test -race ./internal/runlease
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

These are not reported as local passes. `.github/workflows/ci.yml` mandates
Docker-backed PostgreSQL integration and `go test -race -count=1 ./...` on Ubuntu.
