# P4-04 Linear Session Context Builder — Test Report

- Date: 2026-08-23
- Scope: base-version fencing, completed selected history, current user input, immutable snapshot and trust priorities
- Result: PASS for all executable local gates; mandatory PostgreSQL/race execution is checked into Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Linear consistency | Repeatable-read head query binds tenant, Session, current Turn and Run; Builder requires exact Session context version and active Run. |
| Completed history | History SQL joins only completed Turns and their selected Run output; running, failed, cancelled, candidate and internal messages cannot match. |
| Current question | Current running Turn contributes exactly its durable user message and never an in-progress assistant/tool result. |
| Deterministic shape | Completed Turns order by sequence, user before selected assistant; system, history, memory and current-user lanes have stable ordering and explicit retention priorities. |
| Trust isolation | RAG has a reserved lowest-trust lane and cannot be constructed as a system entry; raw Tool/RAG data is absent from conversation history. |
| Immutability | Caller input and returned entry slices are mutated after Build; stored entries and the canonical SHA-256 digest remain unchanged. |
| Corruption defense | Duplicate IDs, incomplete Turn pairs, reversed roles, invalid identity/content and stale ownership fail closed. |
| Real SQL path | Testcontainers seeds completed, failed and running Turns plus selected/superseded candidates; the Snapshot contains only selected completed history and the current user. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/contextmgr statement coverage                      76.7%
context snapshot shuffled repetition (100x)                 PASS
```

The local Windows host cannot execute the remaining platform gates:

```text
AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/repository -run TestContextSnapshotPostgres
  rootless Docker is not supported on Windows

go test -race ./internal/contextmgr ./internal/repository -run TestBuilder|TestContextSnapshot
  go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

These are not reported as local passes. `.github/workflows/ci.yml` mandates
Docker-backed PostgreSQL integration and `go test -race -count=1 ./...` on Ubuntu.
