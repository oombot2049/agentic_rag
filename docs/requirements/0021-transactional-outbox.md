# Requirement P2-07: Transactional outbox and relay

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-07

## 1. Objective

Durably record dispatch intent in the same PostgreSQL transaction as the
business change, then deliver it at least once without holding a database
transaction open during an external publish. The first consumer contract is Run
reservation dispatch, but the storage model is event-type agnostic.

## 2. Atomic write boundary

`outbox.Repository` accepts the same narrow `DBTX` capability as conversation
repositories. `Enqueue` never starts or commits a transaction. The application
constructs both repositories with one caller-owned transaction and writes the
Run plus `run.queued` event before commit. A rollback therefore removes both;
there is no best-effort publish inside the business transaction.

Payloads are copied, valid JSON, non-empty and at most 1 MiB. IDs, tenant,
aggregate type/ID, event type and occurrence time are immutable. Event ID is the
stable delivery/idempotency ID used by downstream consumers.

## 3. Claim and fencing protocol

Each tenant is an explicit relay shard; no repository method performs an
unscoped cross-tenant scan. One SQL statement:

1. selects ready `PENDING` rows in deterministic order;
2. locks only the bounded candidate set with `FOR UPDATE SKIP LOCKED`;
3. increments `claim_token` and `attempt_count`;
4. assigns owner and expiry; and
5. returns the claimed events.

The monotonic token fences a delayed relay after expiry/takeover. Publish,
failure and retry acknowledgements require tenant, event ID, owner, token and an
unexpired claim according to PostgreSQL `CURRENT_TIMESTAMP`. Database time also
decides readiness and computes expiry, so a skewed worker clock cannot steal or
extend a claim. Exact expiry is no longer valid. Claim duration is 5 seconds to
5 minutes and batch size is 1 to 100.

## 4. Relay delivery semantics

The relay claims a batch first, then calls the publisher outside a transaction.
On success it marks the row `PUBLISHED`; on failure it clears the claim and
schedules bounded exponential retry, or marks it `DEAD` after the configured
attempt limit. It processes the rest of the batch and reports per-outcome
counts. Context cancellation stops before another external call.

The guarantee is **at least once**. A crash after publish and before the success
acknowledgement causes redelivery. Publishers propagate the stable event ID and
consumers must deduplicate it or apply an idempotent domain transition. The
outbox does not make an external broker transaction atomic with PostgreSQL.

## 5. Cleanup and retention

Only `PUBLISHED` rows whose `published_at` is strictly older than a supplied
cutoff are eligible. Cleanup is tenant-scoped, bounded to 1,000 rows, ordered by
publication time and uses `SKIP LOCKED` so it does not block active relay work.
Pending, claimed, retrying and dead-letter evidence is never deleted by this
operation. Retention policy selection belongs to operations/configuration.

## 6. Failure handling

- publisher error text is normalized and truncated to 2 KiB before persistence;
- claim/ack zero-row outcomes are classified as not found, fenced or expired;
- malformed database JSON and invalid stored state fail closed during hydration;
- database acknowledgement errors are returned because the event may already
  have been published and must be allowed to redeliver;
- poison events become `DEAD` and remain inspectable; no silent deletion occurs.

## 7. Test strategy

- Scripted SQL tests assert tenant predicates, `SKIP LOCKED`, deterministic
  ordering, token increments, validation, hydration and error classification.
- Relay tests inject publish and acknowledgement failures, verify retry/dead
  routing, stable event identity, cancellation and duplicate-delivery behavior.
- PostgreSQL integration proves business+event commit/rollback atomicity, races
  multiple relay workers without double-claim, exercises expiry takeover and
  verifies cleanup cannot remove non-published rows.

## 8. Acceptance criteria

- [x] Business changes and dispatch intent can share exactly one SQL transaction.
- [x] Concurrent relays use bounded `FOR UPDATE SKIP LOCKED` claims.
- [x] Every acknowledgement is fenced by tenant, owner, token and expiry.
- [x] Delivery is explicitly at least once with stable consumer idempotency ID.
- [x] Retry, dead-letter and bounded published-event cleanup are deterministic.
