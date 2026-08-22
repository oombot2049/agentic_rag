# Requirement P3-03: Create and query Session API

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P2-03 idempotency, P2-05 conversation repositories, P3-02 authentication
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-03

## 1. Objective and contract

Provide `POST /v1/sessions` and `GET /v1/sessions/{session_id}` for the
linear-conversation root aggregate. Both routes use only the authenticated
Principal's tenant. Create requires `sessions:write` and `Idempotency-Key`; get
requires `sessions:read`.

The response exposes `context_version` because clients must carry that value
into later Turn creation. It also exposes `row_version` as an ETag. Context
version represents committed conversation history; row version represents any
concurrent Session mutation and must not be substituted for context version.

## 2. Create transaction and idempotency

The application validates and normalizes the request before computing a
canonical fingerprint over operation `CREATE_SESSION` and `{metadata: ...}`.
Missing metadata and an empty metadata object normalize to the same command.
The idempotency scope is `(tenant_id, authenticated_subject_id, operation, key)`.

```mermaid
sequenceDiagram
    participant C as Client
    participant A as Auth middleware
    participant H as Session handler
    participant S as Session service
    participant DB as PostgreSQL transaction
    C->>A: POST /v1/sessions + credential + Idempotency-Key
    A->>A: verify credential; derive Principal
    A->>H: trusted tenant/subject; credentials stripped
    H->>H: strict JSON and metadata validation
    H->>S: Create(tenant, subject, key, metadata)
    S->>S: normalize + fingerprint + generate ID/time
    S->>DB: begin transaction
    S->>DB: claim idempotency scope
    alt first request
        S->>DB: INSERT session with server audit fields
        S->>DB: complete idempotency record with 201 response
        DB-->>S: commit
    else completed duplicate
        S->>DB: load tenant-scoped recorded Session
        DB-->>S: commit replay
    else same key, different input / still running
        DB-->>S: rollback stable conflict
    end
    S-->>H: Session snapshot
    H-->>C: 201 + Location + ETag
```

The claim, Session insert and completed response are one database transaction.
A crash cannot leave a committed Session with an in-progress idempotency record,
or a replay record pointing at a rolled-back Session. Concurrent duplicates are
serialized by the idempotency primary key. A completed duplicate returns the
original resource, while a changed request receives
`IDEMPOTENCY_KEY_REUSED`. An uncompleted live request receives
`IDEMPOTENCY_IN_PROGRESS`.

## 3. Data ownership and metadata

Session IDs use 128 bits from `crypto/rand`, encoded as `ses_` plus lowercase
hex. The service clock is the sole source of `created_at` and `updated_at`; both
are UTC and identical for a new aggregate. Client input cannot set tenant,
versions, sequence numbers, ownership, IDs or timestamps.

Metadata is an optional string map with at most 20 keys. Keys must be non-empty,
at most 128 bytes, free of control characters and surrounding whitespace;
values are at most 256 bytes and free of control characters. A defensive copy is
stored as JSONB. The database enforces JSON object shape and property count as a
last line of defense. Metadata is deliberately not returned by the current
public `Session` schema and is reserved for server-side correlation/policy.

## 4. Query and non-enumeration

GET parses the typed `ses_` identifier and executes a query constrained by both
trusted tenant ID and Session ID. A foreign-tenant ID and an absent ID return the
same status and safe response body. No global lookup is used to distinguish
those cases. Successful GET responses carry `ETag: "rv-<row_version>"`.

## 5. HTTP boundary

Create accepts only `application/json`, caps the body at 16 KiB, rejects unknown
fields, trailing JSON, missing bodies, duplicate/invalid idempotency keys and
invalid metadata. Responses use JSON, `Cache-Control: no-store`,
`X-Content-Type-Options: nosniff`, `Location` on create and ETag on both routes.
Application failures use the stable error registry and never expose SQL,
credentials, tenant ownership or private causes.

## 6. Test strategy and acceptance

- service tests cover normalized fingerprints, replay, conflict decisions,
  cryptographically generated ID shape, UTC audit fields and store failures;
- HTTP integration tests exercise real authentication/permission middleware,
  strict decoding, headers, validation, idempotent replay and trusted tenant;
- a non-enumeration test proves foreign and absent IDs have byte-identical
  responses;
- concurrent duplicate tests prove one logical Session is returned;
- PostgreSQL integration tests verify atomic claim/insert/complete and tenant
  isolation when Docker is available;
- race, randomized repetition, coverage, vet and static analysis gates apply.

- [x] Create and get are tenant-scoped from trusted context.
- [x] Validation and safe error mapping match the OpenAPI contract.
- [x] New Session audit fields and initial versions are server-controlled.
- [x] `context_version`, Location and row-version ETag are returned.
- [x] Idempotent creation is atomic and concurrent-safe.
