# Requirement P3-04: Atomic Create Turn command

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P2-03 idempotency, P2-04 Session CAS, P2-07 outbox, P3-02 authentication
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-04

## 1. Objective and public contract

`POST /v1/sessions/{session_id}/turns` durably accepts one logical user
question and reserves its initial Agent Run. The caller supplies a trusted
credential, an idempotency key, `expected_context_version`, non-blank message
content and optional `execution.max_steps` (default 12, range 1–20). The 202
response contains Session, Turn, Run and user Message IDs, the allocated Turn
sequence, base context version, `QUEUED` status and resumable events URL.

The first release uses the explicit **REJECT** policy: a Session with an Active
Run returns `SESSION_BUSY`; it does not queue hidden work. No Branch model is
introduced.

## 2. Atomic transaction

```mermaid
sequenceDiagram
    participant C as Client
    participant H as Auth + HTTP
    participant A as Turn service
    participant DB as PostgreSQL transaction
    participant W as Outbox relay / worker
    C->>H: POST Session/turns + expected_context_version + key
    H->>A: trusted tenant/subject + normalized command
    A->>A: validate; default max_steps; fingerprint; generate IDs/time
    A->>DB: begin short transaction
    A->>DB: claim (tenant, subject, CREATE_TURN, key)
    alt acquired
        A->>DB: SELECT tenant-scoped Session
        A->>DB: CAS reserve Active Run + Turn sequence
        A->>DB: INSERT Turn
        A->>DB: INSERT user Message
        A->>DB: INSERT Run(max_steps)
        A->>DB: INSERT RunQueued.v1 outbox event
        A->>DB: complete idempotency response (202)
        DB-->>A: commit
        A-->>H: durable response
        H-->>C: 202 + Location + events_url
        DB-->>W: relay after commit only
    else completed duplicate
        DB-->>A: original stored 202 response
        A-->>H: exact logical replay
    else stale / busy / changed key
        DB-->>A: rollback typed conflict
        A-->>H: stable 409
    end
```

Every write is in the same database transaction. Failure after Session reserve
rolls back its Active Run and sequence together with all child rows and the
idempotency claim. Workers can only observe committed work through Outbox; the
HTTP process never publishes directly to Redis or an in-memory queue.

## 3. Concurrency and conflict classification

The client provides context version, not storage row version. Within the
transaction the adapter loads `(tenant_id, session_id)`, checks context version,
then calls Session CAS with the freshly read row version. The update predicate
also requires no Active Run. If it affects no row, current state is re-read in
this order:

1. absent under the trusted tenant → `SESSION_NOT_FOUND`;
2. context differs → `SESSION_VERSION_CONFLICT`;
3. Active Run exists → `SESSION_BUSY`;
4. otherwise a pure row race is retried a bounded number of times.

This order prevents a stale request from learning unrelated busy state. Two
different commands at the same base version cannot both reserve a Turn. Two
copies of the same idempotent command serialize on the idempotency primary key
and return one response/one Turn.

## 4. Durable records and event

IDs contain 128 bits of CSPRNG entropy and typed prefixes: `trn_`, `msg_`,
`run_`, `evt_`. Server UTC is the only timestamp source. The allocated sequence
comes from Session CAS. `base_context_version` equals the accepted expected
version and Run number is 1.

`runs.max_steps` persists normalized execution policy. The Outbox event has
aggregate `run`, type `RunQueued.v1`, and schema-versioned JSON containing tenant,
Session, Turn, Run, user Message IDs, sequence, base version and max steps. The
payload is an execution notification, not authority: the worker must still use
the event tenant and tenant-scoped repository reads.

## 5. Validation, replay and errors

The HTTP boundary requires one JSON content type/body/key, caps the body at 140
KiB, rejects unknown/trailing/null JSON and parses a typed Session path. Content
is at most 131072 bytes and cannot be blank. Default application occurs before
fingerprinting, so omitted and explicit `max_steps: 12` replay as the same
command. The Session path is part of the fingerprint.

The completed idempotency row stores the original 202 body, Location and Run
resource reference. Replay uses that body rather than re-reading mutable Session
or Run state. Errors use the stable public registry and never expose SQL or
cross-tenant ownership.

## 6. Test strategy and acceptance

- application tests cover normalization, fingerprints, error decisions and
  100 concurrent duplicates;
- HTTP tests use real authentication/authorization middleware and cover strict
  decoding, trusted tenant, headers and conflict codes;
- store fault injection fails after each mutation and proves no partial logical
  result is committed by the transaction abstraction;
- PostgreSQL integration starts competing commands and verifies exactly one
  Message/Turn/Run/Outbox plus correct Session reservation and idempotency row;
- mandatory Linux CI runs the full suite with Docker and race detection.

- [x] Message, Turn, Run, Session reserve, Outbox and idempotency are atomic.
- [x] Context conflict, busy, not-found and idempotency conflict codes are exact.
- [x] Same-command duplicates return one logical response.
- [x] Execution options and base context are durable.
- [x] REJECT policy is explicit and there is no Branch.
