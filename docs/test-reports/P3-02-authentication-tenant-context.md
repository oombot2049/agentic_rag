# P3-02 authentication and tenant context test report

- Date: 2026-08-23
- Commit under test: working tree before P3-02 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0024 authentication and tenant context](../requirements/0024-authentication-tenant-context.md)

## 1. Result

P3-02 passes trusted-tenant provenance, credential ambiguity, immutable Principal,
hashed API-key verification, permission enforcement, non-enumeration, concurrent
request isolation and dual-toolchain quality gates. `internal/auth` has 93.6%
statement coverage.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/auth` | Pass |
| Coverage | `go test -cover ./internal/auth` | Pass; 93.6% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/auth` | Pass |
| Full Go 1.27 | `go test -count=1 ./...`; `go vet ./...`; reproducible build | Pass |
| Full Go 1.26 | `GOTOOLCHAIN=local go test -count=1 ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Credential and identity evidence

Table tests cover Bearer case normalization, API keys, missing values, unsupported
schemes, empty/short values, duplicate fields, comma folding, whitespace/control
characters and simultaneous credential kinds. Only one unambiguous credential is
accepted. Missing adapter and typed-nil adapter/store inputs fail closed.

The middleware test sends valid authentication plus spoofed `X-Tenant-ID` and
`X-User-ID`. The handler receives tenant `ten_0001` from the verifier, while all
credential and spoofed identity headers are absent from its cloned request. The
caller-owned request retains its original header, proving no cross-middleware
mutation.

## 4. Principal and permission evidence

Principal construction rejects malformed tenant, subject and permission values,
deduplicates permissions and copies both input/output collections. Context
round-trip returns another copy. Missing context maps to `UNAUTHENTICATED`; a
valid Principal lacking a route capability maps to `PERMISSION_DENIED`; allowed
requests reach the handler.

One hundred concurrent requests alternate two tokens/tenants and verify that the
path-specific expected tenant always matches request context. No global or shared
mutable current-tenant state exists.

## 5. API-key evidence

The API-key verifier passes only the SHA-256 digest to its store. Raw keys are not
retained. Unknown, revoked, expired and malformed keys all return the same
`ErrInvalidCredential`; store failure becomes `ErrVerifierUnavailable` while
retaining the private cause for diagnostics. Record Principals are revalidated
before context injection.

## 6. Safe failure and non-enumeration evidence

Missing and invalid credentials produce 401/`UNAUTHENTICATED`; verifier outage
produces retryable 503; unexpected errors produce 500. JSON contains no token or
private cause and includes `no-store`, `nosniff`, and the Bearer challenge where
appropriate.

An HTTP authorization matrix requests one absent Session ID and one ID owned by a
different tenant. The handler derives the repository scope only from trusted
context. Both responses have identical 404 status and byte-identical safe JSON,
so ownership cannot be enumerated through response differences.

## 7. Acceptance conclusion

- `X-Tenant-ID` cannot alter or reach downstream authorization: **Pass**.
- Trusted tenant context requires successful authentication: **Pass**.
- Bearer and API-key verifier adapters are independently extensible: **Pass**.
- Coarse permission enforcement is explicit and immutable: **Pass**.
- Cross-tenant authorization is non-enumerable: **Pass**.

P3-02 is complete.
