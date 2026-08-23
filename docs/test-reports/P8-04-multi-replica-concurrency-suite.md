# P8-04 Multi-replica Concurrency Test Report

- Result: Pass for local executable gates; Docker/race scenario mandatory in Linux CI
- Date: 2026-08-23
- Go versions: 1.26.7 and 1.27.0
- Container: `postgres:18.6-alpine`

## Concurrency matrix

| Scenario | Load | Required invariant |
| --- | ---: | --- |
| Same Session, distinct commands | 64 clients / 4 API replicas | one `202`; 63 `SESSION_BUSY`; one Turn/Run/Message/Outbox reservation |
| Different Sessions | 32 clients / 4 API replicas | 32 accepted; 32 distinct active Runs; no tenant-global lock |
| Expired lease takeover | 2 worker authorities | token/version increase; worker A fenced; worker B commits |
| Final replay | 2 identical winner commits | one assistant Message and one `run.completed`; second result is replay |

All API replicas independently construct authentication middleware, application
services, handlers, and repositories. They share only PostgreSQL and
thread-safe test ID generators. Consequently, no handler-local mutex can make
the same-Session assertion pass.

The takeover test uses production `Execution`, `runlease.Manager`,
`FinalCommitService`, and `FinalCommitStore`. Logical clocks cross the five
second minimum lease boundary without a wall-clock sleep. Quiescent test tickers
prevent a heartbeat from renewing worker A during the controlled takeover.

## Local gates

| Gate | Go 1.26.7 | Go 1.27.0 |
| --- | --- | --- |
| `go vet ./...` | Pass | Pass |
| `go vet -tags=concurrency ./tests/concurrency` | Pass | Pass |
| `go test -count=1 ./...` | Pass | Pass |
| tagged concurrency compilation | Pass | Pass |
| tagged `staticcheck` | Pass | Not required |

The Docker-free contract test passes locally. The PostgreSQL scenario skips on
this Windows host because rootless Docker is unsupported. The Linux
`race-integration` job exports `AGENTICRAG_REQUIRE_DOCKER=1` and runs:

```text
go test -race -tags=concurrency -count=1 -v ./tests/concurrency
```

Docker absence, a data race, a leaked shared result, or a scenario failure is
therefore blocking in CI. This report does not represent the local skip as a
race-detector or real-container pass.

## Diagnostics and cleanup

All concurrent callers write exactly one value into a fully sized result
channel before the test aggregates outcomes. The suite uses a three-minute root
deadline and bounded lease-close contexts. On failure it captures at most 256
KiB of PostgreSQL logs, then terminates the container.
