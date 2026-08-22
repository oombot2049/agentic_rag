# Requirement P3-02: Authentication and trusted tenant context

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P0-04 threat model, [P3-01 HTTP server](0023-http-server.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-02

## 1. Objective

Authenticate a request once and place an immutable Principal in request context.
Tenant identity comes only from that Principal. `X-Tenant-ID`, body fields, URL
parameters, model output and tool arguments are untrusted and cannot select or
override the active tenant.

## 2. Principal model

A Principal contains validated tenant ID, authenticated subject ID and a private
permission set. Constructors copy and validate permissions; getters return copies
so downstream code cannot mutate authorization state. Context keys are private to
`internal/auth`, preventing arbitrary packages from manufacturing a trusted
Principal with a string key.

Subject IDs are 1–128 visible safe characters. Permission names are lowercase,
bounded capability strings such as `sessions:read`. There are no implicit admin,
wildcard or role-expansion semantics in this layer.

## 3. Credential boundary

The extractor accepts exactly one of:

- `Authorization: Bearer <token>`; or
- `X-API-Key: <key>`.

Duplicate headers, comma folding, control characters, unsupported authorization
schemes, empty values, oversized values and simultaneous credential kinds all
fail as unauthenticated. The middleware clones headers and removes
`X-Tenant-ID`, `X-User-ID` and `X-Subject-ID` before calling downstream code.

After verification, the raw Authorization/API-key header is also removed before
application handlers to reduce accidental logging or propagation.

`Authenticator` routes credential kinds to explicit `Verifier` adapters. Missing
adapters fail closed. Invalid credentials map uniformly to `UNAUTHENTICATED`;
verified dependency outages map to retryable `SERVICE_UNAVAILABLE`; unexpected
adapter errors map to `INTERNAL`. Raw credentials and private causes never enter
the response.

## 4. API-key adapter

The API-key verifier validates format, hashes the key with SHA-256 immediately,
and supplies only the digest to `APIKeyStore`. The store returns a validated
Principal plus expiry/revocation metadata. Unknown, revoked and expired keys have
the same `ErrInvalidCredential` result. Raw keys are never retained in records,
contexts, logs or error strings.

JWT/OIDC verification is an independent `Verifier` implementation behind the
same port. It must pin algorithms, validate issuer/audience/time and map trusted
claims to `NewPrincipal`; adding it cannot change middleware or tenant lookup
code. This avoids embedding a particular identity vendor in the domain.

## 5. Authorization and non-enumeration

Route-level `RequirePermission` checks coarse capabilities after authentication.
Resource ownership is not implemented by comparing a client-supplied tenant or
returning a special cross-tenant 403. Handlers obtain `TenantID(ctx)` and call
tenant-scoped repositories. A resource absent in tenant A and an ID belonging to
tenant B therefore produce the identical resource-specific not-found response.

Possessing an opaque Session/Run/Artifact ID is never authorization. A trusted
tenant context does not eliminate ACL checks inside knowledge bases or tools;
those later boundaries must further narrow authority.

## 6. Error response contract

The default failure writer uses the centralized `apperror` registry, JSON,
`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, and a Bearer
challenge for authentication failures. It contains stable code, safe message and
retryability only. Transports may inject a compatible writer later to add request
and trace IDs without altering authentication decisions.

## 7. Test strategy

- table tests cover every header ambiguity and malformed credential shape;
- spoofed tenant headers are stripped and cannot change the trusted tenant;
- permission allow/deny and missing-context behavior are verified;
- API-key tests prove only SHA-256 reaches the store and exercise expiry,
  revocation, unknown and dependency failure;
- an HTTP authorization matrix proves absent and foreign-tenant resource IDs have
  byte-identical status/body responses;
- concurrent requests verify Principal isolation under the race gate.

## 8. Acceptance criteria

- [x] Tenant context can only originate from a successful verifier result.
- [x] Untrusted `X-Tenant-ID` is ignored and removed before handlers.
- [x] Bearer and API-key adapters are independently extensible and fail closed.
- [x] Permissions are immutable and route-level enforcement is explicit.
- [x] Cross-tenant and absent resources are non-enumerable at the HTTP boundary.
