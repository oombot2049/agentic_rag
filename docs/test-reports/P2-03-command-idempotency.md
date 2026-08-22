# P2-03 command idempotency test report

- Date: 2026-08-23
- Commit under test: working tree before P2-03 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0017 command idempotency](../requirements/0017-command-idempotency.md)

## 1. Result

P2-03 passes fingerprint, claim classification, SQL repository, validation,
migration, full-repository and static-analysis gates. `internal/idempotency` has
93.8% statement coverage. A 40-contender real PostgreSQL concurrency test is
checked in; local Docker absence is explicit and mandatory-provider mode was
verified to fail rather than skip.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/idempotency ./internal/platform/postgres ./migrations` | Pass |
| Unit + SQL repository coverage | `go test -cover ./internal/idempotency` | Pass; 93.8% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/idempotency ./migrations` | Pass |
| PostgreSQL package | `go test -count=1 ./internal/platform/postgres` | Pass; Docker tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestIdempotencyConcurrentClaimReplayAndReclaim$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet | `go vet ./...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Fingerprint evidence

| Scenario | Expected | Result |
|---|---|---|
| JSON object insertion order differs | Same digest | Pass |
| Path map iteration order differs | Same digest | Pass |
| Typed normalized request vs equivalent generic JSON | Same digest | Pass |
| Integer above JavaScript exact range | No float precision loss | Pass |
| Operation, path name/value or body changes | Different digest | Pass |
| Unsupported body value / invalid operation / empty path name | Validation failure | Pass |
| Caller mutates returned digest bytes | Stored fingerprint unchanged | Pass |

The test input contains only operation, path and normalized body; request ID,
trace ID and headers have no representable fingerprint field.

## 4. Repository and state evidence

Scripted `database/sql` driver tests exercise actual repository SQL call paths:

- inserted/reclaimed row returns `ACQUIRED`;
- insert conflict plus load classifies in-progress, successful replay, final
  failure replay and different-hash key reuse;
- query/row corruption and disappearing-record paths fail closed;
- completion accepts exactly one affected row, propagates database failures and
  returns `ErrClaimLost` for zero rows;
- nil executor, malformed tenant/subject/operation/key, zero hash/time, TTL below
  24 hours, invalid JSON/status/resource pairs and oversized response all fail.

The PostgreSQL integration suite concurrently releases 40 claimers against the
same composite key/hash and requires exactly one `ACQUIRED` plus 39
`IN_PROGRESS`. It then verifies successful replay payload/resource fidelity,
different-hash conflict, repeated-completion fencing, final-error replay and
same-key reclamation at exact expiry with a new hash.

## 5. Migration evidence

Migration 000002 defines a tenant-leading composite primary key, exact 32-byte
request hash, closed states, JSON response and response-shape checks, bounded
scope fields, minimum 24-hour expiry, `timestamptz` instants and tenant-leading
expiry index. Static migration tests verify these fragments and the paired Down
file. The P2-02 full migration test now rolls down both versions and reapplies
both, preventing later migrations from invalidating the original round trip.

## 6. Acceptance conclusion

- Same key/hash replays the original stable response: **Pass**.
- Different hash under the same scope/key is classified as reuse: **Pass**.
- Atomic claim permits one execution owner under concurrency: **Pass**.
- Completion and expired reclamation are fenced: **Pass**.
- Scope is exactly tenant + subject + operation + key: **Pass**.
- Canonical fingerprint excludes transport-only identity: **Pass**.

P2-03 is complete.

