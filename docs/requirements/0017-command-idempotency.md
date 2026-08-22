# Requirement P2-03: Command idempotency and canonical request fingerprint

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-03

## 1. Objective

Guarantee that retrying one durable command cannot create duplicate Turns/Runs
when the client did not receive the first response. Identity is the exact tuple
`(tenant_id, authenticated_subject_id, operation, idempotency_key)`; no subset
is safe. The component does not deduplicate SSE reconnects or downstream model,
retrieval and tool attempts.

## 2. Fingerprint contract

The application validates the request and applies defaults before calling the
fingerprinter. Its input contains only the normalized operation, canonical path
parameters and typed body. Request/trace IDs, transport headers and secrets are
never fields of this input.

Path keys are sorted. The normalized body is JSON-encoded, decoded with exact
JSON number tokens, then encoded again so object key order and formatting do not
change identity. Operation, every path key/value and body are hashed with
length-prefixed framing and SHA-256, avoiding delimiter ambiguity. The digest is
a typed 32-byte `Fingerprint`; logs may use its hex form but never the body.

Defaults are deliberately not invented by the generic hasher: operation-specific
request normalization owns them. Thus omitted `max_steps` and explicit `12`
produce the same hash only after both become the same typed command.

## 3. Record and state machine

```text
missing/expired --claim--> IN_PROGRESS --complete success--> SUCCEEDED
                                   \--complete final error--> FAILED_FINAL
```

| Existing record | Incoming hash | Decision |
|---|---|---|
| none or expired | any valid | `ACQUIRED`; caller may execute exactly once |
| `IN_PROGRESS` | same | `IN_PROGRESS`; map to 409 |
| `SUCCEEDED` | same | `REPLAY_SUCCEEDED` with stored response |
| `FAILED_FINAL` | same | `REPLAY_FAILED_FINAL` with stored response |
| any live state | different | `KEY_REUSED`; map to 409 |

`Claim` uses one `INSERT ... ON CONFLICT ... DO UPDATE ... WHERE expired`
statement. The unique primary key serializes concurrent claimers; only the
insert/reclaimer receives a row. A loser then reads the committed record and
classifies it using constant-time hash comparison. An expired key is reset to
`IN_PROGRESS` with all prior response fields cleared.

Completion updates only a matching `IN_PROGRESS` row with the same hash. Zero
affected rows returns `ErrClaimLost`; it never overwrites a replay or a command
that reclaimed an expired key. Create-Turn application code must bind a
Repository to the same SQL transaction that creates its business rows.

## 4. Stored response

Successful and final-error outcomes persist HTTP status, valid bounded JSON
body, optional Location and optional resource type/ID. This is the stable replay
snapshot, not a live reconstruction that could drift after later state changes.
`FAILED_FINAL` means the exact command will never be executed again under this
record; transient infrastructure failures leave/reconcile `IN_PROGRESS` under a
separate recovery policy.

Records expire no earlier than 24 hours after creation. The application may use
a longer configured TTL. Expiry permits a future new claim but is not a signal
for clients to intentionally recycle keys.

## 5. Schema and tenant safety

`idempotency_records` has a tenant-leading composite primary key, 32-byte hash
check, closed statuses, response-shape checks, timezone-aware timestamps and a
tenant-leading expiry index. Subject, operation and key are bounded and reject
control characters. Response bodies are `jsonb`; no request body or secret is
stored. The migration is paired and reversible as version 000002.

## 6. Test strategy

- Table tests cover key/scope/TTL/response validation and all decision states.
- Fingerprint tests cover JSON/path ordering, default-normalized equality,
  operation/path/body differences, exact number handling and copy safety.
- PostgreSQL integration starts many concurrent claimers for one scope/hash and
  requires exactly one `ACQUIRED`; same-hash losers observe `IN_PROGRESS`.
- Integration then verifies different-hash conflict, success/final-error replay,
  stored response fidelity, lost-claim fencing and expired-record reclamation.
- Migration tests verify tenant key, hash/status/response constraints, expiry
  index and `up -> down -> up` compatibility.

## 7. Acceptance criteria

- [x] Same scope/key/hash returns the original durable result.
- [x] Same scope/key with a different hash produces a deterministic reuse conflict.
- [x] Concurrent claimers cannot both acquire execution ownership.
- [x] Completion is fenced by scope, hash and `IN_PROGRESS` state.
- [x] Retention is at least 24 hours and expired records can be safely reclaimed.
- [x] Request fingerprints are canonical and exclude transport correlation IDs.

