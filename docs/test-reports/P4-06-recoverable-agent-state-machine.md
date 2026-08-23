# P4-06 Recoverable Agent State Machine — Test Report

- Date: 2026-08-23
- Scope: Agent Loop phases, Step/Attempt boundaries, checkpoint recovery, fencing and PostgreSQL transaction atomicity
- Result: PASS for all executable local gates; mandatory PostgreSQL/race execution is checked into Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Step/Attempt boundary | Ordinary and takeover retries retain one Step and stable operation key while appending a distinct, monotonically numbered Attempt. |
| Durable transitions | Begin, retry, complete, fail and loop-complete each map to one Store call and one PostgreSQL transaction containing the checkpoint write. |
| No long transaction | The executor asserts the Store transaction flag is false; a Begin rollback yields zero external invocations. |
| Fault isolation | Injected rollback at all five durable boundaries prevents the machine from reporting or executing a later phase. |
| Crash takeover | An EXECUTING checkpoint converts the abandoned STARTED Attempt to UNKNOWN before Attempt N+1 is invoked. |
| Unknown outcome safety | A directive that forbids unknown retry persists terminal failure and performs no replay; retryable directives reuse the same operation key. |
| CAS fencing | A repository row-version conflict causes `runlease.Handle` loss and prevents the external call. |
| Bounded execution | Attempt exhaustion and persisted Run `max_steps` both stop execution; no extra Step is inserted. |
| Checkpoint validation | Unknown fields/schema, trailing JSON, impossible phase shapes, foreign ownership and invalid IDs/versions fail closed. |
| PostgreSQL rollback | Checked-in Testcontainers test forces invalid checkpoint failure after child writes and asserts Step/Attempt counts remain zero, then verifies atomic success states. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/agent statement coverage                           66.8%
state machine/checkpoint deterministic repetition (100x)    PASS
```

The package coverage includes the real PostgreSQL adapter while Docker-backed
paths are skipped on this host. The forced platform gates were attempted and
are not reported as local passes:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/agent -run TestPostgresStoreAtomicStepBoundaries
  rootless Docker is not supported on Windows

go test -race ./internal/agent -run TestMachine|TestCheckpoint
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

`.github/workflows/ci.yml` mandates Docker-backed PostgreSQL integration and
`go test -race -count=1 ./...` on Ubuntu.
