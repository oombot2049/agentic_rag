# P2-01 core domain aggregates test report

- Date: 2026-08-23
- Commit under test: working tree before P2-01 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0015 domain aggregates](../requirements/0015-domain-aggregates.md)

## 1. Result

P2-01 passes its state-machine, aggregate-invariant, tenant-ownership,
concurrency-version, persistence-independence and full-repository regression
gates. `internal/domain` has 80.1% statement coverage; exhaustive transition
matrices contribute 129 named Run/Step/Attempt state-edge cases.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/domain` | Pass |
| Domain unit + coverage | `go test -cover ./internal/domain` | Pass; 80.1% |
| State nondeterminism | `go test -shuffle=on -count=100 ./internal/domain` | Pass |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet | `go vet ./...` | Pass |
| Go 1.27 build | `go build -buildvcs=false -trimpath -o bin/agenticrag.exe ./cmd/agenticrag` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Workflow syntax | `actionlint .github/workflows/ci.yml` | Pass |
| Patch hygiene | `git diff --check` | Pass |

Docker integration tests use the P1-08 policy: they skip only on this local host
where Docker is unavailable and are mandatory in CI. The Linux CI race job runs
the entire repository with `go test -race`; this Windows host has no supported C
compiler, so the local evidence does not claim a race-detector execution.

## 3. State-machine evidence

| Aggregate | Evidence |
|---|---|
| Turn | Running completion/failure/cancellation; all three terminal states reopen only by appending the next queued Run |
| Run | Full 9×9 source/target matrix; legal edges accepted, every other edge rejected without mutation |
| Step | Full 6×6 source/target matrix plus successful, failed, skipped and cancelled public method paths |
| Attempt | All 4 source states against 3 terminal outcomes; only `STARTED` may finish and prior outcomes remain immutable |
| Message | User visibility fixed; assistant candidate promotes once; repeated/invalid promotion rejected |

## 4. Aggregate and concurrency evidence

| Scenario | Expected | Result |
|---|---|---|
| Stale create-Turn command while another Run could be active | Version conflict is decided before busy | Pass |
| Second active Run in one Session | `ErrSessionBusy`; no sequence/version mutation | Pass |
| Successful final commit | Message promoted, Turn completed, Run selected, context/head advanced, active Run cleared | Pass |
| Foreign-tenant final participant | Typed validation failure; all four participants unchanged | Pass |
| Regeneration of committed head | New Run appended under same Turn; `next_turn_sequence` unchanged | Pass |
| Failed/cancelled/timed-out Run release | Active ownership cleared; context version unchanged | Pass |
| Wrong or nonterminal release | Rejected without Session mutation | Pass |
| Attempt numbering | Starts at one, remains contiguous and unique | Pass |
| Returned ID collection | Caller mutation cannot alter aggregate state | Pass |
| Numeric boundary | Versions and sequences over signed 64-bit range rejected before mutation | Pass |

## 5. Architecture evidence

The architecture test parses every non-test Go source file in `internal/domain`
and rejects imports of `database/sql`, pgx, Redis, `internal/platform` or API
transport packages. Production domain code currently imports only `errors`,
`fmt`, `strings` and `time` from the standard library. Snapshot types contain
plain domain values and timestamps, with no SQL nullable wrappers or struct tags.

## 6. Acceptance conclusion

- Illegal state transitions are rejected without partial mutation: **Pass**.
- Session linear concurrency and dual-version semantics are modeled: **Pass**.
- Tenant/parent ownership and ordered child invariants are enforced: **Pass**.
- One Turn supports multiple Runs and no Branch type exists: **Pass**.
- Domain model has no database/transport dependency: **Pass**.
- Table-driven state and aggregate tests plus repository gates pass: **Pass**.

P2-01 is complete.

