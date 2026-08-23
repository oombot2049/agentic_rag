# P3-07 Resumable SSE Gateway — Test Report

- Date: 2026-08-23
- Scope: `internal/transport/sse`, `internal/events`, Redis Stream primitives, migration `000008`
- Result: PASS for all executable local gates; Docker/race gates are mandatory in checked-in Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Disconnect replay | `TestHandlerReplaysAfterLastEventIDAndClosesOnTerminal` resumes strictly after the cursor through a frozen high-water mark. |
| Duplicate suppression | `TestHandlerDeduplicatesLiveOverlap` feeds the last durable event again through live tail and observes each ID exactly once. |
| Heartbeat/fallback | `TestHandlerHeartbeatAndRedisFailureFallback` proves heartbeats remain flushed while Redis is unavailable and PostgreSQL polling remains active. |
| Slow consumer | `TestHandlerDisconnectsBlockedSlowClient` blocks a real `ResponseWriter`, overflows the one-entry connection queue, releases the writer, and requires prompt handler exit. |
| Cursor retention | Handler tests assert 410 `EVENT_CURSOR_EXPIRED`; the PostgreSQL integration test deletes a bounded expired prefix and requires `retained_from=11`. |
| Cross-tenant isolation | Handler and repository tests use trusted tenant identity and require foreign and absent Runs to have the same 404 response. |
| Redis live tail | Broker tests require tenant-hash-tagged keys, exact `<sequence>-0` IDs, strict identity validation and explicit TTL; Valkey Testcontainers verifies XADD/XREAD. |
| Durable ordering | `TestEventRepositoryConcurrentAppendReplayRetentionAndTenantIsolation` performs 20 concurrent atomic appends and requires the complete ordered sequence. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: staticcheck ./...                                  PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
SSE package: go test -cover                                 PASS (79.4%)
SSE package: go test -count=20                              PASS
Migration contract tests                                   PASS
```

Local Windows cannot execute the remaining platform gates:

```text
go test -race ./internal/transport/sse
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1

AGENTICRAG_REQUIRE_DOCKER=1 go test ...
  rootless Docker is not supported on Windows
```

This is not recorded as a local pass. `.github/workflows/ci.yml` enforces
`AGENTICRAG_REQUIRE_DOCKER=1`, PostgreSQL/Valkey integration tests and
`go test -race -count=1 ./...` on Ubuntu.
