# P2-07 transactional outbox test report

- Date: 2026-08-23
- Commit under test: working tree before P2-07 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0021 transactional outbox](../requirements/0021-transactional-outbox.md)

## 1. Result

P2-07 passes transactional boundary, SQL claim/fencing, relay failure injection,
stable duplicate identity, retry/dead-letter, bounded cleanup, migration and
dual-toolchain quality gates. `internal/outbox` has 82.2% statement coverage.
The real PostgreSQL atomicity and 8-worker/40-event contention test is checked
in; local Docker absence is explicit and mandatory mode fails rather than skips.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/outbox ./internal/platform/postgres ./migrations` | Pass |
| Outbox coverage | `go test -cover ./internal/outbox` | Pass; 82.2% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/outbox ./migrations` | Pass |
| PostgreSQL package | `go test ./internal/platform/postgres` | Pass; Docker tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestTransactionalOutboxAtomicityConcurrentClaimsTakeoverAndCleanup$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Race detector | `CGO_ENABLED=1 go test -race ./internal/outbox` | Not runnable locally: no C compiler; mandatory Linux CI runs `go test -race ./...` |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet/build | `go vet ./...`; `go build -buildvcs=false -trimpath ./...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Repository and SQL evidence

Scripted `database/sql` tests execute and inspect all primary statements:

- enqueue copies valid JSON and writes only immutable event fields;
- every operation starts from an explicit typed tenant and event identity;
- claim uses a deterministic bounded CTE with `FOR UPDATE SKIP LOCKED`;
- readiness, claim expiry, acknowledgement validity and retry scheduling use
  PostgreSQL `CURRENT_TIMESTAMP`, avoiding worker-clock lease theft;
- each claim increments monotonic token and attempt count;
- publish/failure acknowledgement requires current owner/token and unexpired
  claim, with not-found, expired and fenced outcomes distinguished;
- cleanup deletes only tenant-scoped `PUBLISHED` rows older than cutoff, with a
  hard 1,000-row bound and `SKIP LOCKED`;
- malformed IDs, JSON, names, times, limits, durations, owners and hydrated
  lifecycle state fail before use or fail closed.

## 4. Relay and failure-injection evidence

A deterministic batch contains one successful event, one transient publisher
failure and one poison event at the final attempt. The relay publishes the first,
schedules exponential retry for the second and marks the third dead while
reporting exact outcome counts. Publisher error strings are UTF-8 safely bounded
to 2 KiB.

Another test injects a database failure after a successful external publish.
The relay returns the acknowledgement error. A later worker receives the same
stable event ID with a higher claim token and publishes it again, explicitly
demonstrating the expected at-least-once duplicate and consumer idempotency
contract.

## 5. PostgreSQL concurrency specification

The checked-in integration test first injects failure after inserting one
business Session and one outbox event in the same transaction and proves both
roll back. It then commits the same pair and proves both become visible.

Eight workers concurrently claim 40 ready events in batches of ten. The test
requires all 40 unique IDs exactly once across the active claims. A separate
event is expired, taken over with token two, and rejects the token-one worker
before accepting the new worker. Cleanup removes the published event but leaves
a pending event, and a foreign tenant cannot read it.

## 6. Migration evidence

Migration 000004 creates `outbox_events` with tenant-scoped identity, bounded
JSON, lifecycle/claim shape checks, nonnegative attempts/token, and tenant-leading
partial indexes for ready delivery, cleanup and dead-letter inspection. Static
contract tests verify the paired migration. The full schema round trip now rolls
all four feature migrations down and reapplies them.

## 7. Acceptance conclusion

- Business state and dispatch intent share one caller-owned transaction: **Pass**.
- Concurrent claims are bounded, nonblocking and token-fenced: **Pass**.
- Relay retries and dead-letters without hiding publish/ack failures: **Pass**.
- Duplicate delivery preserves the stable consumer idempotency ID: **Pass**.
- Published cleanup is tenant-scoped, bounded and lifecycle-safe: **Pass**.

P2-07 is complete.
