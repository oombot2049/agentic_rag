# Requirement P3-08: Idempotent Run Cancellation and Safe Points

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P2-06 execution repository, P3-05 Run query API
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-08

## 1. Boundary and API

`POST /v1/runs/{run_id}:cancel` is an explicit command; closing an SSE/HTTP
connection is never cancellation. The endpoint requires trusted tenant identity,
`sessions:write` and one 16..128-byte `Idempotency-Key`. It accepts no query or
request body. The fingerprint binds operation, tenant-scoped Run path and an
empty body. A replay returns the byte-identical durable response.

The response is always HTTP 202 after a successful command decision:

```json
{
  "run_id": "run_...",
  "status": "CANCEL_REQUESTED",
  "cancellation_requested": true,
  "terminal": false
}
```

Foreign and absent Runs both return `RUN_NOT_FOUND`. A different command body or
path under the same key returns `IDEMPOTENCY_KEY_REUSED`; concurrent ownership of
the same key returns `IDEMPOTENCY_IN_PROGRESS`.

## 2. State decisions

| Locked Run state | Command result | Session/Turn effect |
|---|---|---|
| `QUEUED` | `CANCELLED`; emit `run.cancel_requested`, then `run.cancelled` | Clear matching Active Run; increment Session `row_version`; cancel running Turn |
| `RUNNING` | `CANCEL_REQUESTED`; emit `run.cancel_requested` | Keep Active Run; increment Session `row_version` for cancellation bookkeeping |
| `CANCEL_REQUESTED` | No state/event mutation; return current state | None |
| `CANCELLED` | No state/event mutation; return terminal state | None |
| Any other finished state | No mutation; `cancellation_requested=false` | None; completed work is immutable |

Every real transition increments `runs.execution_version`. The command never
clears an active worker lease for a running Run. This allows that worker to
observe the new version/status, while every stale Step/Attempt/checkpoint write
is fenced.

## 3. Safe-point protocol

```mermaid
sequenceDiagram
    participant C as Client
    participant A as Cancel API
    participant P as PostgreSQL
    participant W as Agent worker
    participant T as External tool
    C->>A: POST Run:cancel + idempotency key
    A->>P: lock Run; RUNNING -> CANCEL_REQUESTED; execution_version++
    P-->>A: durable response + event/outbox
    A-->>C: 202 CANCEL_REQUESTED
    alt tool call was already dispatched
        T-->>W: outcome
        W->>P: persist/reconcile outcome under current authority
        Note over W,P: never roll back or relabel a completed side effect
    end
    W->>P: safe point with tenant/Run/lease owner/token
    P->>P: lock and validate lease; CANCEL_REQUESTED -> CANCELLED
    P->>P: clear Session Active Run; cancel Turn; append event/outbox
    P-->>W: stop=true, new execution_version
```

A safe point is immediately before creating the next Step and after reconciling
the preceding Attempt. The worker supplies tenant, Run and lease owner/token.
The acknowledgement transaction locks the Run, requires the lease to remain
owned and unexpired, and returns the latest `execution_version` for the next
authorized mutation. This lets the legitimate worker observe the cancel-command
version bump while stale lease tokens receive `RUN_FENCED` and stop.

The execution repository already requires this authority on Step/Attempt and
checkpoint writes. Therefore:

- cancel before the next Step mutation: the version bump fences that mutation;
- cancel after an external side effect began: it is allowed to finish and must
  be reconciled; the worker reloads the Run to obtain the bumped execution
  version, persists the Attempt outcome under the still-owned lease, and only
  then enters the safe point; cancellation never issues compensating rollback;
- at the next safe point: Run becomes `CANCELLED`, the Turn becomes `CANCELLED`,
  and the Session is released atomically;
- completed Step, Attempt, ToolCall, RetrievalResult and Artifact records are
  append-only/immutable and are not rewritten by cancellation.

## 4. Atomic persistence and ordering

The public command runs in one PostgreSQL transaction:

1. claim the scoped idempotency key;
2. lock the tenant-scoped Run row;
3. decide from its current state;
4. CAS Run and matching Session/Turn control rows;
5. append durable SSE event(s) and transactional outbox notification(s);
6. persist the exact response and commit.

The safe-point acknowledgement uses the same Run-first row-lock order and then
Session/Turn, preventing deadlock inversion. Event sequence allocation and
insertion use the P3-07 single-statement append. A rollback leaves no state,
event, outbox or idempotency partial result.

## 5. Race invariants and tests

- 50 concurrent same-key calls produce one transition and one response.
- Different-key concurrent cancellation is naturally idempotent: at most one
  request event and one final cancellation event are emitted.
- Cancel racing Step start has only two valid serializations: Step start wins and
  its side effect is reconciled, or cancellation version fencing wins and Step
  start fails before dispatch.
- A stale/expired/foreign worker cannot acknowledge cancellation.
- Foreign tenant cancellation is indistinguishable from an absent Run.
- Fault injection before idempotency completion rolls back state and events.
- Client/SSE disconnect does not call the cancellation service.

## 6. Acceptance checklist

- [x] Cancel command is durable and exact-replay idempotent.
- [x] `RUNNING -> CANCEL_REQUESTED -> CANCELLED` occurs only through a safe point.
- [x] `QUEUED -> CANCELLED` atomically releases Session and Turn.
- [x] Stale workers and next-Step races are fenced by execution version.
- [x] Completed or in-flight side effects are never falsely rolled back.
- [x] Cross-tenant and concurrency integration tests are checked into mandatory CI.
