# Requirement P6-04: Durable Tool Approval Workflow

- Status: Complete
- Priority: P1
- Owner: Go Backend
- Last reviewed: 2026-08-23
- Depends on: P6-02 Tool Call Ledger, P6-03 Execution Boundary
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P6-04

## 1. Objective

Pause a Run before a policy-gated Tool Call without holding a Worker, lease,
goroutine or queue delivery. Persist an approval capability bound to the exact
operation; an independent human decision either requeues the same Run or
terminates it safely.

```mermaid
sequenceDiagram
  participant W as Worker
  participant DB as PostgreSQL
  participant H as Human/API
  participant Q as Durable queue
  W->>DB: Request approval + fenced Run/Step/Attempt
  DB->>DB: INSERT WAITING approval; Run -> WAITING_APPROVAL; clear lease
  DB-->>W: commit (Worker releases delivery)
  H->>DB: decide(approval, actor, nonce, row_version)
  alt approved before expiry
    DB->>DB: approval -> APPROVED; Run -> QUEUED; outbox resume event
    DB-->>Q: relay after commit
  else rejected or expired
    DB->>DB: approval terminal; Attempt/Step/Run -> FAILED; clear active Session Run
  end
```

## 2. Approval capability binding

The immutable request binds tenant, requesting subject, allowed approver
subject, Session/Turn/Run/Step/Attempt and ToolCall IDs, exact tool name/version
and effect, canonical argument SHA-256, creation/expiry and a random 256-bit
nonce hash. PostgreSQL stores only the nonce hash. Approval ID and nonce are
both required; neither is authorization alone.

A decision actor must exactly match the bound approver. Changing any operation
field creates a conflict rather than reusing approval. Cross-tenant reads and
decisions return not-found semantics. Nonce comparison is constant-time.

## 3. State and ownership

Approval statuses are WAITING, APPROVED, REJECTED and EXPIRED. Only WAITING may
transition, using row-version CAS. The Run gains nonterminal
`WAITING_APPROVAL`; entering it requires the current unexpired Worker lease and
atomically clears the lease while advancing execution fencing version. Thus no
Worker owns a Run during human think time.

Approval requeues the Run and emits one durable resume notification. Rejection
or expiry marks the active Attempt and Step failed, marks the Run failed and
clears the Session active Run in one transaction. Expiry is decided from server
UTC during either a decision or a bounded sweeper; a late approval can never
revive the Run.

## 4. Idempotency and concurrency

`(tenant, Run, Step)` admits one approval. Exact request replay returns the
same public capability only to the original creation transaction; the raw nonce
is never recoverable from storage. Decision replay with the same terminal state
returns the stored record, while a conflicting decision is rejected. Row locks
serialize approve/reject/expire contenders and exactly one terminal path emits
events or performs Run transitions.

## 5. Test strategy

Domain tests cover binding, expiry boundaries, actor/nonce checks, exact replay,
conflicting decisions and defensive copies. PostgreSQL fault injection covers
every mutation boundary. Testcontainers races approval versus rejection and
expiry, verifies lease release while waiting, one resume event on approval, and
safe Run/Step/Attempt/Session termination otherwise. Tenant matrix tests prove
non-enumeration.

## 6. Acceptance checklist

- [x] Approval binds actor and the exact tenant/Run/Step/Tool/argument operation.
- [x] WAITING_APPROVAL is durable and holds no Worker lease or process resource.
- [x] Approval before expiry requeues once through a durable event.
- [x] Rejection/expiry safely terminates Attempt, Step and Run atomically.
- [x] Late, stale, replayed, mutated and cross-tenant decisions fail closed.
- [x] Human identity, timeout and decision state remain tenant-queryable/auditable.
