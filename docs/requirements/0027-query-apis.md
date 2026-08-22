# Requirement P3-05: Turn, Run and message-history query APIs

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P2-05 conversation repositories, P2-06 execution repository, P3-02 authentication
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-05

## 1. Objective

Expose tenant-scoped safe reads for `GET /v1/turns/{turn_id}`,
`GET /v1/runs/{run_id}` and
`GET /v1/sessions/{session_id}/messages`. Reads report durable state only; they
do not attach to a worker, mutate leases or infer state from SSE/queue presence.

## 2. Tenant isolation and public projections

Handlers derive tenant exclusively from the authenticated Principal and call
ports that require it. Every SQL identity predicate begins with tenant ID.
Foreign-tenant and absent IDs therefore return byte-identical resource-specific
404 responses.

Turn projection includes ordered Run candidates and exposes `selected_run_id`
only after selection. Run projection exposes terminal state and final Message ID
only when durable domain state has one. Lease owner/token, execution version,
checkpoint, candidate content and provider/tool diagnostics are private and
never appear in public JSON. Terminal `completed_at` is the durable update time;
otherwise it is null. Fields not represented durably, such as a distinct start
instant or reason code, remain null rather than being guessed.

## 3. Conversation visibility

Message history is sourced from `messages` with
`visibility = 'CONVERSATION'`. This returns the user question immediately and
only the selected/final assistant Message after final commit promotion.
`CANDIDATE` assistant answers and `INTERNAL` system/tool traces are excluded in
SQL, not filtered after loading. An existing empty Session returns an empty page;
an absent Session returns 404.

## 4. Stable opaque cursor

Ordering is ascending `(created_at, message_id)`. The repository requests
`page_size + 1` and the next cursor represents the last returned row, preventing
duplicates when timestamps tie. No offset is used.

The cursor is base64url-encoded, versioned binary data containing UTC nanoseconds
and typed Message ID, authenticated with HMAC-SHA256. Its MAC also binds trusted
tenant and Session ID. A cursor copied to another tenant/Session, modified,
truncated, oversized, non-canonical or signed with an old/unknown key fails as
`INVALID_ARGUMENT`. The signing key is injected at process composition and must
contain at least 32 bytes; it is never returned or logged.

## 5. HTTP behavior

All routes require `sessions:read`. `page_size` defaults to 50 and is bounded
1–100; duplicate query values and unknown query parameters are rejected.
Successful responses use JSON, `no-store` and `nosniff`. Timestamps are UTC RFC
3339 Nano. Safe errors use the stable registry.

## 6. Test strategy and acceptance

- projection tests cover every terminal/non-terminal and optional-ID shape;
- cursor tests cover round-trip, equal timestamps, tamper, cross-session,
  cross-tenant, non-canonical and key validation;
- HTTP tests use real authentication and prove stable page traversal and
  byte-identical non-enumeration;
- repository/PostgreSQL tests prove candidate/internal exclusion and stable
  `(created_at,id)` traversal under equal timestamps.

- [x] Turn and Run state is tenant-scoped and contains no worker-private fields.
- [x] History returns conversation-visible user and selected/final messages only.
- [x] Pagination is deterministic, opaque, authenticated and bound to ownership.
- [x] Foreign and absent resources are not distinguishable.
