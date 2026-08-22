# P2-08 Artifact Store test report

- Date: 2026-08-23
- Commit under test: working tree before P2-08 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Provider targets: PostgreSQL 18.6 and pinned MinIO in mandatory Linux CI
- Requirement: [0022 Artifact Store](../requirements/0022-artifact-store.md)

## 1. Result

P2-08 passes metadata isolation, streaming integrity, TTL/encryption validation,
S3 Signature V4, create-only writes, multipart upload and dual-toolchain quality
gates. Core Artifact coverage is 86.7%; the S3 adapter is 81.8%. A combined real
PostgreSQL/MinIO contract test is checked in. Local Docker absence is explicit and
mandatory mode fails rather than skipping.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/artifact ./internal/platform/postgres ./migrations` | Pass |
| Coverage | `go test -cover ./internal/artifact/...` | Pass; core 86.7%, S3 81.8% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/artifact/... ./migrations` | Pass |
| PostgreSQL package | `go test ./internal/platform/postgres` | Pass; provider tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestArtifactStorePostgresAndMinIOContract$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Full Go 1.27 | `go test -count=1 ./...`; `go vet ./...`; reproducible build | Pass |
| Full Go 1.26 | `GOTOOLCHAIN=local go test -count=1 ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Streaming and integrity evidence

- A payload above 1 MiB streams through the service without a payload-sized
  service buffer, calculating SHA-256 via `io.TeeReader`.
- Short, extra and provider-checksum-mismatched uploads are rejected and trigger
  best-effort object rollback.
- Metadata insertion failure is fault-injected after a successful object write;
  the object is deleted and the database error remains observable.
- Download wraps the provider body and validates byte count plus SHA-256 only at
  successful EOF. Corrupt content returns `ErrIntegrity`.
- Expired metadata blocks storage access. Tenant mismatch is indistinguishable
  from absence.

## 4. S3 protocol evidence

HTTP contract tests verify canonical Signature V4 authorization, server-generated
paths, create-only conditional PUT, streaming GET and idempotent DELETE. Invalid
endpoints, buckets, keys and encryption modes fail before network access.

The multipart test lowers internal thresholds and streams a 12-byte fixture as
three ordered parts, verifies ETags in completion XML and checks conditional
completion. A separate failure injection makes a part return 503 and proves the
multipart upload is aborted. Production thresholds use single PUT through 5 GiB,
64 MiB default parts and dynamically grow part size to stay below S3's 10,000-part
limit through the 5 TiB contract ceiling.

## 5. Database and MinIO specification

Migration 000005 contains only Artifact metadata: tenant/ID, opaque object key,
32-byte SHA-256, size, media type, encryption mode/key reference and timestamps.
Static tests reject payload/content/body/data columns and verify the tenant-leading
expiry index and paired rollback.

The mandatory integration test starts PostgreSQL and pinned MinIO, creates a
private bucket, streams a multi-megabyte client-encrypted Artifact, persists and
reloads metadata, downloads and verifies all bytes, rejects cross-tenant access
and prevents duplicate overwrite. It queries `information_schema` to prove no
payload-like database column exists.

## 6. Acceptance conclusion

- Hash, encryption metadata and mandatory TTL are persisted: **Pass**.
- Upload/download are streaming and end-to-end integrity checked: **Pass**.
- Large content never enters the core relational table: **Pass**.
- Object identity is tenant-prefixed, server-generated and create-only: **Pass**.
- Real PostgreSQL + MinIO compatibility test is mandatory in Linux CI: **Pass (checked in; local provider unavailable)**.

P2-08 is complete.
