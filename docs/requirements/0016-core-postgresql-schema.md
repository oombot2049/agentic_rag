# Requirement P2-02: Core PostgreSQL schema and reversible migration

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-06 PostgreSQL foundation](0012-postgresql-foundation.md), [P2-01 domain aggregates](0015-domain-aggregates.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-02

## 1. Objective

Create the first production PostgreSQL migration for Session, Turn, Message,
Run, Step and Attempt. The schema must preserve P2-01 ownership, ordering and
state invariants, make tenant omission difficult, support the frozen read/write
paths, use `timestamptz` for every instant, and roll fully down and back up.

This migration intentionally does not add Branch. Idempotency, outbox/events,
leases, Artifacts, ToolCalls and RetrievalResults are owned by later tasks and
must not be smuggled into this schema increment.

## 2. Tenant-key strategy

Every business table has `tenant_id NOT NULL` as the first primary/foreign-key
component. Resource IDs are not treated as authorization and are not globally
unique database keys:

```text
primary identity       (tenant_id, id)
parent relationship    (tenant_id, parent_id, ...)
repository lookup      WHERE tenant_id = $1 AND id = $2
```

Hierarchical foreign keys include enough parent columns to prevent valid IDs
from different Sessions or Turns being combined. For example, a Run references
`(tenant_id, session_id, turn_id)` and a Message attached to a Run references
`(tenant_id, session_id, turn_id, run_id)`. PostgreSQL therefore rejects a
cross-tenant or cross-parent row even if application validation is bypassed.

The migration does not create a Tenant table. Tenant identity and membership
belong to the external control-plane boundary in the MVP; `tenant_id` remains a
mandatory trusted scope on every data-plane record.

## 3. Tables and invariants

| Table | Primary relationships | Important constraints |
|---|---|---|
| `sessions` | tenant root for a linear conversation | non-negative context version; positive row/next versions; committed sequence below next; optional active Run/head Turn |
| `turns` | belongs to Session; owns user Message and Run candidates | positive unique sequence per Session; frozen status set; selected Run belongs to same Turn |
| `messages` | belongs to Turn; optional producing Run | completed immutable row; user has no Run and is conversation-visible; candidate is assistant-only |
| `runs` | belongs to Session and Turn | positive unique `run_no` per Turn; frozen Run states; output Message required after success |
| `steps` | belongs to Run | positive unique `step_no`; frozen kind and state sets |
| `attempts` | belongs to Step | positive unique `attempt_no`; terminal attempts require `ended_at`; started attempts forbid it |

Circular final-commit references—Session head/active Run, Turn user/selected Run,
and Run output Message—are installed after table creation as `DEFERRABLE
INITIALLY DEFERRED` foreign keys. This permits the command and final-commit rows
to be inserted/updated in one transaction while still enforcing ownership at
commit. Deleting a parent cascades only down the containment tree; selection and
head references use `NO ACTION` to avoid ambiguous reverse cascades.

All IDs enforce the P2-01 8–128 ASCII resource syntax and type prefix. Numeric
versions and sequences are `bigint` with non-negative/positive checks, matching
the signed 64-bit public contract. Every timestamp column is `timestamptz` and
mutable records require `updated_at >= created_at`.

## 4. Index design by query

| Query path | Index |
|---|---|
| Session control read/CAS | primary key `(tenant_id,id)`; row version remains a predicate on that one row |
| Find busy Sessions | partial `sessions(tenant_id,active_run_id)` |
| Conversation Turn page | unique `(tenant_id,session_id,turn_sequence)` ordered backwards by the same prefix |
| Conversation Message history | partial `(tenant_id,session_id,created_at,id)` for `CONVERSATION` |
| Turn candidates | unique `(tenant_id,session_id,turn_id,run_no)` |
| Worker/session active Run scan | partial `(tenant_id,session_id,updated_at,id)` for nonterminal states |
| Ordered Run steps | unique `(tenant_id,run_id,step_no)` |
| Ordered Step attempts | unique `(tenant_id,step_id,attempt_no)` |
| Unknown-outcome reconciliation | partial `(tenant_id,ended_at,id)` for `UNKNOWN` |

Foreign-key leading-column indexes are explicit where they are not already
covered by a unique constraint. Indexes never omit tenant scope, except their
predicate constants.

## 5. Migration and deployment protocol

Files are embedded into the binary-facing `migrations` package and follow the
P1-06 `000001_name.up.sql` / `.down.sql` convention. P1-06 supplies the advisory
lock, checksum ledger and per-file transaction. Deployment expands this initial
schema before any repository binary uses it. Down drops constraints/tables in
reverse dependency order but intentionally leaves the shared migration ledger.

The migration contains no `CONCURRENTLY` operation because the P1-06 runner
executes each file transactionally and this is an empty-schema bootstrap. Later
large-table index changes require a separately reviewed online-migration plan.

## 6. Test strategy

- Static SQL contract tests inspect embedded files, exact table set, mandatory
  tenant columns, `timestamptz`, state checks, composite ownership keys, indexes,
  absence of Branch and a reverse-order down file.
- PostgreSQL Testcontainers applies the real migration, introspects tables,
  columns, constraints and indexes, inserts a complete deferred-FK aggregate,
  demonstrates same resource IDs can exist in two tenants, and proves a foreign
  tenant parent reference fails.
- The integration suite executes `up -> down -> up`, verifies all six tables are
  removed and recreated, and verifies repeated Up is idempotent.
- Docker absence skips locally only; `AGENTICRAG_REQUIRE_DOCKER=1` makes it a CI
  failure under the mandatory P1-08 integration/race job.

## 7. Acceptance criteria

- [x] All six core tables contain mandatory `tenant_id` and composite identity.
- [x] Parent ownership cannot cross tenant, Session, Turn, Run or Step boundaries.
- [x] Unique constraints and tenant-leading query indexes cover frozen access paths.
- [x] Domain state, sequence, timestamp and output/visibility invariants are checked.
- [x] Every instant uses `timestamptz`; no database type enters `internal/domain`.
- [x] Real PostgreSQL migration round trip and failure tests are present.
- [x] No Branch table, column or relationship exists.

