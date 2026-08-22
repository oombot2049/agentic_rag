# Requirement P2-06: Run, Step, Attempt, lease and checkpoint repository

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md), [P2-04 Session CAS](0018-session-version-control.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-06

## 1. Objective

Persist Agent execution history and guarantee that only the current leased
worker can advance a Run. A stopped, partitioned or slow worker must be fenced
after lease takeover even if it later resumes with cached state.

## 2. Fencing model

Three independent counters have distinct scope:

| Counter | Scope | Purpose |
|---|---|---|
| `runs.execution_version` | Run control row | CAS for every lease/status/checkpoint control mutation |
| `runs.lease_token` | lease generation | monotonically increases on acquire/takeover; old workers can never regain authority |
| `steps/attempts.row_version` | execution history row | CAS for the one legal state transition of that row |

A lease identity is `(tenant_id, run_id, lease_owner, lease_token,
execution_version, lease_expires_at)`. Worker mutations require all identity
fields plus `lease_expires_at > command_time`. Owner strings are opaque workload
instance IDs, never authentication credentials.

## 3. Lease operations

- Acquire/takeover requires expected Run version and a missing or expired lease,
  increments both execution version and lease token, and sets bounded expiry.
- Renew requires current owner, token and version while the lease is unexpired;
  it increments execution version but preserves token.
- Release requires current owner/token/version, clears owner/expiry and increments
  execution version. The token is retained so it is never reused.
- Zero-row writes load the current tenant-scoped record and classify not found,
  lease held, fenced token/owner or CAS conflict without leaking another tenant.

Lease duration is bounded between 5 seconds and 5 minutes. Database time is not
implicitly consulted: the caller supplies one UTC command instant consistently
to predicates and returned expiry, enabling deterministic testing.

## 4. Step, Attempt and checkpoint writes

Step insertion and transition, Attempt insertion and terminal outcome, and
checkpoint save all carry the owning Run's expected execution version plus the
current unexpired lease owner/token. If Run authority is absent, no child row is
written. Step/Attempt transitions additionally match their own row version and
state; terminal history cannot be rewritten. Lease/control mutations increment
the Run version, while child writes preserve it so one leased loop iteration can
atomically append related history under one authority snapshot.

Attempts append contiguously under a Step. Checkpoints are one current row per
Run and move only forward by `step_no`; equal step number is allowed for a
deterministic overwrite by the same fenced execution generation. Payload is
valid JSON, at most 1 MiB, and contains resumable orchestration state—not raw
large prompts/results, which belong in Artifacts.

## 5. Schema migration

Migration 000003 adds Run execution/lease columns, Step and Attempt row versions,
a tenant-scoped `run_checkpoints` table and a tenant-leading lease-scan index.
Checks enforce closed lease shapes, positive versions/tokens, bounded owner and
JSON payload size. Down removes the table, index, constraints and columns in
reverse order.

## 6. Repository boundaries

`repository.Execution` accepts caller-owned `DBTX`, returns domain Run/Step/
Attempt values plus persistence control metadata, and never starts a hidden
transaction. Multi-statement workflows must bind it to `postgres.WithinTx`.
Every statement requires tenant ID; there is no ID-only lookup/update API.

## 7. Test strategy

- Table/scripted SQL tests cover hydration, lease validation, every failure
  classification, version/token propagation and child/checkpoint fencing.
- PostgreSQL integration races many workers on one expected version and requires
  one lease owner, verifies renew, expiry takeover and rejection of the old
  owner/token.
- Integration advances Step and Attempt through legal states, rejects stale row
  versions, saves/loads a checkpoint, rejects backward checkpoints and proves a
  released/expired lease cannot write.
- Migration tests cover columns, checks, index, tenant checkpoint identity and
  full `up -> down -> up` across all versions.

## 8. Acceptance criteria

- [x] Lease acquire, renew, release and expired takeover are CAS-safe.
- [x] Old owner/token cannot mutate after takeover.
- [x] Step and Attempt updates require both child version and current Run lease.
- [x] Checkpoints are tenant-scoped, bounded, monotonic and lease-fenced.
- [x] All execution updates carry version plus lease ownership predicates.
