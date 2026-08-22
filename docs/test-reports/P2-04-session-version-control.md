# P2-04 Session version control test report

- Date: 2026-08-23
- Commit under test: working tree before P2-04 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0018 Session version control](../requirements/0018-session-version-control.md)

## 1. Result

P2-04 passes deterministic conflict classification, CAS SQL, snapshot hydration,
validation, full-repository and static-analysis gates. `internal/session` has
83.2% statement coverage. A real PostgreSQL 100-transaction contention test is
checked in; local Docker absence is explicit and mandatory-provider behavior
was verified to fail rather than skip.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/session ./internal/platform/postgres` | Pass |
| Unit + SQL coverage | `go test -cover ./internal/session` | Pass; 83.2% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/session` | Pass |
| PostgreSQL package | `go test -cover ./internal/platform/postgres` | Pass; Docker tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestSessionHundredWayReservationCAS$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet/build | `go vet ./...`; `go build -buildvcs=false -trimpath ...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Unit and SQL evidence

Scripted `database/sql` tests execute the repository's real query paths and
cover:

- successful reservation returns sequence one and the incremented row snapshot;
- stale context wins classification even if the Session is also busy;
- current-context active owner returns busy;
- same context/no active owner with a changed row returns CAS conflict;
- missing tenant-scoped row returns not found;
- regeneration preserves next sequence;
- final commit increments both versions;
- release increments only row version;
- wrong active owner returns run-fenced before row-version classification;
- invalid IDs, zero row/sequence/time and corrupt persisted references fail;
- SQL errors are wrapped without being confused with business conflicts.

All SQL predicates include tenant ID and Session ID. Reservation and completion
guard signed-64-bit overflow and timestamp reversal in the update itself.

## 4. 100-way PostgreSQL contention specification

The checked-in integration test creates one empty Session, captures context zero
and row version one, then releases 100 goroutines simultaneously. Every goroutine
opens its own SQL transaction and attempts the same-version reservation with a
unique Run ID. Only the CAS winner inserts its deferred-FK Turn, user Message and
Run before commit. Expected result:

```text
ACQUIRED transaction: 1
ErrBusy transactions:  99
next_turn_sequence:     2
context_version:        0
row_version:            2
active_run_id:          winner
```

The test then commits the winner and verifies context/row become 1/3; a request
using context zero returns version conflict. It reserves a regeneration without
consuming another Turn sequence, releases it so context remains one and row
becomes five, verifies stale row returns CAS conflict, and verifies a different
tenant cannot load the Session.

Correctness is database-backed: no Go mutex or shared winner flag participates
in reservation.

## 5. Acceptance conclusion

- Expired context returns a distinct version conflict: **Pass**.
- Exactly one of 100 concurrent reservations can succeed: **Pass**.
- Losing contenders do not consume sequence/version: **Pass**.
- Context and row versions advance under different rules: **Pass**.
- Worker completion/release are active-owner and row-version fenced: **Pass**.
- All operations are tenant-scoped and hydrate validated domain state: **Pass**.

P2-04 is complete.

