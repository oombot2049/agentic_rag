# Requirement P2-05: Session, Turn and Message repositories

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-05

## 1. Objective

Persist and hydrate Session, Turn and Message domain aggregates without allowing
an unscoped ID lookup or hiding transaction boundaries. PostgreSQL remains the
conversation source of truth; repositories return domain objects, not database
rows or JSON transport models.

## 2. Transaction boundary

`internal/repository.Conversation` is constructed from a narrow `DBTX` supplied
by the caller. It never begins, commits or rolls back a transaction. Application
code may bind it to the shared Pool for an isolated read or to the exact SQL
transaction containing Session CAS, idempotency, Turn, Message, initial Run and
outbox changes. Multi-query Turn hydration requires a caller-owned read
transaction when a point-in-time snapshot is required.

No LLM, retrieval, tool, queue or object-store call belongs inside that database
transaction.

## 3. Tenant and ownership rules

- Every method requires a typed `TenantID`; there is no `GetByID(id)` API.
- Every SQL statement starts its identity predicate with `tenant_id`.
- Inserts persist the tenant and complete parent identity already validated by
  P2-01; composite database foreign keys provide the second line of defense.
- A missing row under the supplied tenant returns `ErrNotFound`, whether the ID
  is absent or belongs to another tenant. This avoids an existence oracle.
- SQL/constraint causes remain private and are wrapped by operation; transport
  mapping occurs later.

## 4. Repository operations

| Aggregate | Writes | Reads |
|---|---|---|
| Session | insert validated empty/restored Session | load by tenant + Session ID through the P2-04 hydrator |
| Turn | insert Turn metadata | load by tenant + Turn ID plus ordered Run IDs |
| Message | insert immutable content metadata | load by tenant + Message ID; page committed conversation history |

Turn Run IDs are loaded from `runs` ordered by `run_no`; the Turn row does not
duplicate a mutable array. Message history uses keyset pagination on
`(created_at,id)`, filters `CONVERSATION` in SQL, orders oldest-first and reads
`limit+1` to produce a stable next cursor. Limit is 1–100. Candidate/internal
Messages cannot leak through the normal history method.

## 5. Hydration and write safety

SQL nullable references become optional typed IDs only after prefix validation.
Statuses, roles, visibility, timestamps, sequences and parent IDs pass through
the P2-01 restore constructors; corrupted rows fail closed. Inserts use getters
and copy content into bound parameters. No `SELECT *`, string-built SQL,
database struct tags or tenant-free helper is allowed.

## 6. Test strategy

- Scripted SQL tests assert query text/arguments include tenant scope, exercise
  inserts, not-found behavior, hydration, ordered Run IDs, keyset limit+1,
  hidden candidate filtering and corrupt-row rejection.
- PostgreSQL integration inserts identical Session/Turn/Message IDs in two
  tenants and proves each repository returns only its scoped content.
- Transaction integration rolls back a multi-aggregate insert and proves no
  partial Session/Turn/Message remains, then commits and reads it back.
- Pagination integration verifies no duplicates/gaps across equal timestamps
  because ID is the deterministic tie-breaker.

## 7. Acceptance criteria

- [x] No repository method or SQL lookup can omit tenant identity.
- [x] Transaction ownership is explicit and repositories never auto-commit a workflow.
- [x] Session, Turn and Message hydrate through validated domain constructors.
- [x] Conversation history excludes candidate/internal Messages and is keyset paginated.
- [x] Cross-tenant identical IDs remain isolated and missing scope fails closed.

