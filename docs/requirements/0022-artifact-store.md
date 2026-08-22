# Requirement P2-08: Artifact metadata and object storage

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P2-02 core schema](0016-core-postgresql-schema.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P2-08

## 1. Objective

Store large immutable prompts, retrieval evidence and tool/provider payloads
outside PostgreSQL while preserving tenant ownership, integrity, encryption and
retention evidence in the authoritative database. PostgreSQL contains metadata
only; byte streams use the provider-neutral `ObjectStore` port.

## 2. Data model and identity

Artifact IDs use the `art_` resource prefix. Object keys are generated only by
the service as `tenants/{tenant}/artifacts/{artifact}`; client paths are never
accepted. Metadata records tenant, object key, SHA-256, byte size, media type,
encryption mode/key ID, creation time and mandatory expiry. The object key and
integrity fields are immutable after insertion.

Encryption modes are `SSE-S3`, `SSE-KMS` and `CLIENT`. KMS and client-side
encryption require a bounded, non-secret key reference; SSE-S3 prohibits one.
Buckets remain private, credentials and signed
URLs are not persisted, and TTL must be positive and no more than ten years.

## 3. Streaming upload protocol

The application supplies a declared byte length. `Service.Upload` validates all
metadata, wraps the input in `io.LimitedReader` and `io.TeeReader`, and streams
directly to the object adapter while calculating SHA-256. It rejects short input,
extra bytes, provider size mismatch and provider checksum mismatch. Only after
verification does it insert metadata, making the Artifact visible to domain
readers. A metadata failure triggers best-effort object deletion; reconciliation
must remove any orphan left by a simultaneous provider failure.

The service never buffers the complete payload and PostgreSQL never receives
payload bytes. The maximum accepted size is configuration at the application
boundary; the core contract caps one object at 5 TiB.

## 4. Streaming download protocol

Download first resolves metadata by `(tenant_id, artifact_id)` and rejects an
expired Artifact before contacting storage. The returned `io.ReadCloser` hashes
and counts bytes as the caller consumes them. At EOF it verifies both stored
length and SHA-256; close errors are joined. Consumers therefore cannot treat a
truncated or corrupted stream as successfully complete.

Cross-tenant lookup returns the same not-found result as absence. The object key
is not a bearer authorization mechanism.

## 5. Object-store contract

The port supports streaming put/get and idempotent delete. Put returns provider
observed size and checksum. Options carry media type plus encryption intent; an
adapter must fail when the selected provider cannot honor encryption. The S3
adapter uses audited Signature V4 requests, create-only writes and stable
metadata headers. Objects above 5 GiB use sequential streaming multipart upload;
part size grows automatically to remain within S3's 10,000-part limit, completion
is conditional, and every failed multipart session is aborted.

MinIO is a development compatibility target, not the production authority. Its
contract test runs in the mandatory Docker CI job and covers streaming upload,
download, metadata, checksum and delete without weakening AWS S3 portability.

## 6. Failure and lifecycle rules

- metadata is inserted only after object verification;
- duplicate tenant/Artifact identity fails without overwriting an object;
- database failure attempts object rollback and returns both errors when needed;
- expiry blocks new reads but does not pretend bytes were deleted;
- lifecycle deletion/reconciliation operates from metadata and is idempotent;
- hashes, sizes and IDs may be logged, but contents and encryption key material
  must not be logged.

## 7. Test strategy

- unit tests use short/extra/corrupt/error streams and a recording object store;
- repository tests validate tenant scoping and corrupt-row rejection;
- PostgreSQL integration proves metadata isolation and payload absence;
- MinIO Testcontainers contract tests exercise the real S3-compatible adapter;
- multi-megabyte tests use readers that fail if an implementation attempts an
  unbounded read, demonstrating streaming rather than whole-object buffering.

## 8. Acceptance criteria

- [x] PostgreSQL stores metadata/hash only, never Artifact payload bytes.
- [x] Upload and download are streaming and integrity checked.
- [x] Tenant, TTL and encryption metadata are mandatory and validated.
- [x] Object keys are server-generated and tenant-prefixed.
- [x] S3/MinIO behavior is covered by a mandatory provider contract test.
