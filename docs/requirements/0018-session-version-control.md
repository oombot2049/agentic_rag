# Requirement P2-04: Session dual-version and CAS control

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-04

## 1. Objective

Provide the tenant-scoped SQL control operations that serialize a linear
Session. `context_version` expresses committed conversation history to clients;
`row_version` is an internal optimistic-lock/fencing token. They are never
interchangeable and no in-process mutex is relied upon.

## 2. Version semantics

| Operation | Context version | Row version | Other control state |
|---|---:|---:|---|
| Reserve new Turn Run | unchanged | +1 | consume next sequence; set active Run |
| Reserve regeneration | unchanged | +1 | keep Turn sequence; set active Run |
| Release failed/cancelled Run | unchanged | +1 | clear matching active Run |
| Final commit | +1 | +1 | advance/reaffirm head and committed sequence; clear active Run |

A repository caller reads a Session snapshot and supplies both the client's
expected context version and the snapshot row version. SQL updates include
tenant, Session ID, both expected versions and active ownership in their `WHERE`
clause. Zero rows are classified by a fresh tenant-scoped read.

## 3. Deterministic conflict order

Reservation failure classification is fixed:

1. absent scoped row -> `ErrNotFound`;
2. actual context differs -> `ErrVersionConflict`;
3. active Run is present -> `ErrBusy`;
4. row version differs -> `ErrCASConflict`;
5. otherwise fail closed as `ErrCASConflict`.

This preserves the P0 contract: an old client observes version conflict after a
commit, while a client on the current context observes busy during execution.
Commit/release first verify active Run ownership (`ErrRunFenced`) and then the
worker's row version (`ErrCASConflict`). A stale worker must stop and cannot
blindly retry final commit.

## 4. Transaction boundary

`internal/session.Repository` binds to a narrow `DBTX`. The application creates
it over the same SQL transaction used to insert Turn/Message/Run/outbox rows.
The reservation's deferred active-Run foreign key is therefore satisfied before
commit. Network, LLM, retrieval and tool calls never occur in this transaction.

Each update returns the complete new Session snapshot through `RETURNING` and is
validated by `domain.RestoreSession`; corrupt persisted state fails closed.
Timestamps are caller-injected UTC instants and cannot move backwards.

## 5. SQL operations

- `Load` always filters by `(tenant_id,id)`.
- `ReserveTurn` checks expected context/row, empty active owner and signed-64-bit
  headroom, then atomically increments `next_turn_sequence` and `row_version`.
- `ReserveRegeneration` additionally requires the current committed head and
  does not consume a sequence.
- `CommitRun` requires matching active Run and row version, accepts either the
  newly reserved sequence or current-head regeneration, advances context once
  and clears ownership.
- `ReleaseRun` requires matching active Run and row version, clears ownership
  and leaves context/head unchanged.

## 6. Test strategy

- Scripted SQL tests cover every zero-row classification order, scan validation,
  returned reservation sequence and completion/release fencing.
- PostgreSQL integration launches 100 transactions with the same initial
  context/row snapshot. Exactly one reserves and commits its Turn/Message/Run;
  all others observe busy and no sequence is consumed by losers.
- Integration verifies stale context after final commit, stale row fencing,
  release without context movement, regeneration without sequence movement and
  cross-tenant not-found behavior.
- Repetition and race CI exercise goroutine safety; correctness comes from the
  database CAS, not shared Go state.

## 7. Acceptance criteria

- [x] Stale client context is distinguished from internal CAS failure.
- [x] At most one concurrent reservation succeeds for a Session.
- [x] Losing reservations do not consume sequence or increment versions.
- [x] Final commit/release require active Run ownership and row fencing.
- [x] Context advances only for final commit; row version advances on every control write.
- [x] Every query/update is tenant-scoped and returns validated domain snapshots.

