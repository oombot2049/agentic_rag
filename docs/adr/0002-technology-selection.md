# ADR-0002: Select the MVP technology baseline behind explicit ports

- Status: Accepted
- Date: 2026-08-23
- Decision owners: Architecture, backend and SRE
- Related documents: [capacity](../capacity/workload-baseline.md), [threat model](../security/threat-model.md), [API contract](../api/domain-and-api-contract.md)

## 1. Context

AgenticRAG needs a concrete, reproducible stack for Go development, durable
multi-tenant state, transient coordination, queueing, large immutable Artifacts
and hybrid retrieval. The MVP capacity target is 50 accepted Turns/s, 3,000
concurrent Runs, 20,000 SSE connections and up to 50 million searchable chunks.

Infrastructure products evolve faster than domain semantics. The choice must be
specific enough to test while preserving an exit path through repository-owned
ports. “Vendor neutral” without a reference implementation is not testable;
hard-coding vendor types into domain packages is not replaceable.

## 2. Decision summary

| Capability | Reference choice | Compatibility baseline | Domain-facing port |
|---|---|---|---|
| Language/toolchain | Go 1.27 toolchain | `go 1.26` language/module baseline; CI on latest 1.26 and 1.27 patch | Go interfaces and standard context/errors |
| Final database | PostgreSQL 18, current minor | PostgreSQL 16–18 | Repositories and application transaction manager |
| SQL driver | `pgx/v5`, pinned | PostgreSQL wire protocol through `database/sql` where useful | No pgx types outside platform package |
| Cache/transient stream | Valkey 9.1 reference; tests currently pin 9.1.0 | Tested Redis-protocol subset; managed Redis/Valkey only after conformance | `Cache`, `EventStream`, typed key codec |
| Run queue | PostgreSQL lease queue + transactional outbox | At-least-once, `SKIP LOCKED`, fencing and idempotent consumers | `RunQueue`, `OutboxPublisher` |
| Object storage | S3 API | AWS S3 reference; compatible providers require contract suite | `ArtifactStore` |
| Hybrid/vector retrieval | OpenSearch 3.x, pin 3.8 in reference environment | Explicit index mapping and API contract | `SearchIndex` and domain `Retriever` |
| Knowledge MCP | MCP adapter, not a database | Versioned capability/schema contract | `Retriever` adapter and Tool Gateway |

Go modules and integration-test images are pinned to exact versions. The
repository does not yet contain complete production release manifests or image
digests; those remain a release-engineering requirement. `latest` tags are
prohibited outside a human-facing example.

## 3. Go version policy

### Decision

- Develop and release with Go 1.27.x, using the current security/bug-fix patch.
- Set the module language baseline to Go 1.26 so consumers on the other officially
  supported Go family are not excluded without need.
- CI tests the current patch of Go 1.26 and 1.27. The primary lint/race/release
  job uses 1.27.
- Upgrade the language baseline only when a required language/library feature
  has an accepted ADR and both CI families cannot support it.

The Go release policy supports a major release until two newer major releases
exist. Go 1.27.0 was released on 2026-08-19; versions are rechecked by dependency
automation rather than copied indefinitely from this ADR.

### Alternatives

- Rust: strong safety and performance, but higher delivery cost for the intended
  Go backend ecosystem and no requirement that justifies the switch.
- Java/Kotlin: mature service stack, but larger runtime/operational footprint and
  conflicts with the explicit Go project requirement.
- Go 1.27 language baseline immediately: simpler, but needlessly prevents the
  supported 1.26 toolchain from building the module.

### Exit strategy

Go is a deliberate source-level commitment. Provider protocols and data schemas
must remain language-neutral enough for a future service to be implemented
elsewhere, but no “easy language swap” is claimed.

## 4. PostgreSQL as final source of truth

### Decision

Use PostgreSQL 18 current minor in reference CI/development and support majors
16–18. PostgreSQL stores Session/Turn/Run/Step/Attempt/Message state,
idempotency, leases, Artifact metadata, durable events/outbox and audit indexes.

Reasons:

- transactions and constraints fit final commit, version CAS and tenant-scoped
  referential integrity;
- `FOR UPDATE SKIP LOCKED`, advisory locks and transactional outbox support the
  initial queue/migration design;
- broad managed-service availability and a documented five-year major support
  policy reduce operational lock-in;
- one authoritative store avoids distributed transactions in the MVP.

Driver-specific values stay inside `internal/platform/postgres`. Application
ports use domain IDs, time, primitive values and typed errors. SQL migrations are
forward reviewed, reversible where safe and verified against every supported
major before release.

### Alternatives

- MySQL: capable relational database, but the team would give up the selected
  PostgreSQL locking/index/JSON conventions without a compensating requirement.
- Distributed SQL: useful for multi-region writes, which are an explicit MVP
  non-goal; it adds consistency and operational complexity.
- Document database: weakens relational constraints around linear concurrency,
  idempotency and final commit.

### Exit strategy

PostgreSQL is the hardest component to replace because transaction semantics are
intentional. Repositories isolate queries, but a replacement must pass the same
constraint, isolation, CAS, outbox and recovery suite. Data leaves through
versioned logical exports/backfills with dual-read verification—never by assuming
SQL products are behaviorally interchangeable.

## 5. Valkey/Redis-compatible transient services

### Decision

Use Valkey 9.1 as the open reference implementation for cache, rate-limit state
and SSE/event-stream acceleration. The supported contract is a deliberately
small Redis-protocol command subset, not “all Redis features.” Managed Redis or
Valkey offerings are admitted only after the same conformance/fault suite.

Valkey 9.1 has an announced maintenance/security support window and a permissive
project license. Official latest is 9.1.1 as of this review, while conformance
tests remain pinned to 9.1.0; upgrading that fixture requires the normal fault
and compatibility run. Production manifests should pin an approved patch and
digest. Do not store final
conversation state, sole idempotency records or sole durable events in it.

Key rules:

- keys are built by typed codecs and always include environment and tenant;
- values carry schema version and, for sensitive lookups, tenant checksum;
- TTL is mandatory unless a registry explicitly documents persistence;
- cache miss/failure falls back to PostgreSQL or fails predictably, never to an
  unscoped key;
- Lua/functions and vendor modules require an ADR because they narrow portability.

### Alternatives

- Redis product editions: protocol-compatible deployments may be used after
  license/procurement and conformance review; they are not assumed equivalent.
- In-process cache: useful only for immutable bounded data; inconsistent across
  replicas and unsuitable for shared rate/SSE state.
- PostgreSQL for all transient state: possible but increases hot-row and
  connection pressure without improving final-state correctness.

### Exit strategy

`Cache` and stream ports plus namespaced codecs make Redis data disposable.
PostgreSQL replay reconstructs the lifecycle live tail; transient output instead
falls forward to the authoritative final Message. Migration is dual-write,
compare, cut read, then expire old keys. No business table stores a Valkey stream
ID as its only durable cursor.

## 6. PostgreSQL lease queue and transactional outbox

### Decision

The MVP Run queue is a PostgreSQL-backed lease queue. Reservation and an outbox
record commit in the same transaction. Workers claim ready rows with bounded
batches and `FOR UPDATE SKIP LOCKED`, record a lease owner/expiry and use a
monotonic fencing version on every mutation. Delivery is at least once;
idempotency is a consumer/domain responsibility.

Why this is appropriate now:

- 50 Runs/s and 3,000 concurrent Runs are inside a benchmarkable relational
  workload, avoiding a second durability system before evidence requires it;
- command reservation cannot commit without durable dispatch intent;
- recovery and operational inspection use the same authoritative identifiers;
- `RunQueue` is still an explicit port, so an external broker is not hidden in
  application code.

Queue tables are partitioned/cleaned by lifecycle; workers use randomized/alerted
poll intervals and connection pools so empty polling cannot dominate PostgreSQL.
Outbox publication and queue consumption have separate idempotency records.

### Alternatives

- NATS JetStream: strong low-latency operational choice, but adds another durable
  cluster and still requires transactional outbox bridging.
- Kafka/Redpanda: excellent replay and high throughput, excessive for initial
  command rate and operational footprint.
- Cloud queues such as SQS/Pub/Sub: managed and scalable, but make the reference
  project cloud-specific and still need outbox/idempotency.
- Valkey Streams: useful event acceleration, but not selected as sole Run
  dispatch durability.

### Exit triggers and strategy

Add an external queue adapter when any trigger remains true in two representative
tests or a production window:

- sustained accepted Run rate exceeds 200/s;
- queue polling/locking consumes more than 10% database CPU or I/O budget;
- p95 ready-to-lease delay exceeds 1 second below 70% worker utilization;
- queue recovery or independent regional scaling cannot meet the approved SLO.

The outbox remains the handoff boundary. Migration publishes old and new queue
adapters with one delivery ID, compares consumer lag, moves consumers by shard,
then drains the PostgreSQL queue. Consumer fencing/idempotency is unchanged.

## 7. S3 API for Artifact storage

### Decision

Use the S3 API as the `ArtifactStore` reference contract. AWS S3 is the production
reference; S3-compatible services must pass contract tests for conditional put,
checksum, ranged read, metadata, delete, multipart upload, server-side encryption
and signed URL behavior.

Artifact keys are server-generated and tenant-prefixed for defense in depth.
Buckets are private. PostgreSQL stores tenant, immutable content hash, size,
media type, encryption/retention metadata and object key. A write becomes visible
to domain code only after checksum verification and metadata commit. Orphan
uploads and missing objects are reconciled.

The local unit-test adapter is memory/filesystem based and not a production
provider. The project does not force operators to accept a particular third-party
S3-compatible server license.

### Alternatives

- PostgreSQL large objects/bytea: simplify infrastructure but inflate backup,
  WAL and hot database I/O for large prompts and tool/retrieval payloads.
- Provider-specific Blob/GCS APIs: valid production targets behind new adapters;
  not the portable reference contract.
- Shared filesystem: poor multi-replica durability and cloud portability.

### Exit strategy

`ArtifactStore` uses provider-neutral object operations and opaque object keys.
Migration copies by manifest, verifies size/hash/metadata, dual-reads during a
bounded window, then switches the active store. Signed URLs are regenerated and
never persisted as durable object identity.

## 8. OpenSearch hybrid/vector search

### Decision

Use OpenSearch 3.x, with 3.8 pinned in the reference environment, for online
keyword/vector candidate retrieval and hybrid score combination. It matches the
50-million-chunk planning scale and provides a single index surface for BM25,
metadata/ACL filtering, vector search and hybrid ranking.

The application owns:

- versioned index templates and aliases;
- explicit tenant, knowledge-base, document-version and ACL fields;
- deterministic embedding model/version/dimension metadata;
- query/filter construction and post-result tenant/ACL validation;
- `SearchIndex` adapter and higher-level `Retriever` contract;
- offline relevance evaluation independent of engine scores.

OpenSearch Agents/MCP features are not used as the AgenticRAG domain/runtime.
Only search APIs cross the adapter boundary. This prevents an infrastructure
product from owning Run, tool or conversation semantics.

### Alternatives

- PostgreSQL + pgvector: attractive for smaller deployments and fewer systems;
  retained as a future adapter, but not the default for the planned 50 million
  chunks and hybrid-search operational profile.
- Dedicated vector databases: may outperform for a particular scale/filter
  profile; they add another lexical-search solution or fusion layer.
- Vendor-hosted search/vector API: operationally simple, but increases data,
  API and pricing lock-in; acceptable behind the same contract after security and
  relevance tests.

### Exit triggers and strategy

Re-evaluate when any condition persists after tuning:

- p95 retrieval exceeds 400 ms below 70% provisioned load;
- Golden Set Recall@K/MRR misses its gate;
- projected chunks exceed 100 million or tenant concentration breaks shard
  isolation;
- index/storage/operations cost violates the approved unit-cost budget;
- required data residency or provider availability cannot be met.

Migration builds a new versioned index from immutable document/chunk versions,
dual-queries a sampled shadow workload, compares ACL safety and relevance, swaps
the alias/adapter, then retires the old index after rollback retention.

## 9. Knowledge MCP placement

A knowledge base may participate through MCP, but MCP is an adapter behind
`Retriever`, not the search database or domain contract. The server injects
tenant/ACL scope, validates tool capability/schema, applies deadlines and
validates returned document/chunk identities. A direct OpenSearch adapter and an
MCP adapter can coexist and feed the same fusion/rerank pipeline.

This placement permits customers to expose proprietary knowledge through MCP
without allowing model-provided arguments to choose another tenant or bypass the
repository's evidence model.

## 10. Package and dependency boundaries

```text
internal/domain                 no infrastructure SDK imports
internal/application            domain ports and use cases
internal/platform/postgres      pgx, SQL, migrations
internal/platform/redis         Redis-protocol client and codecs
internal/platform/objectstore   S3 SDK adapter
internal/platform/search        OpenSearch HTTP/client adapter
internal/queue                  RunQueue implementations
internal/retrieval              Retriever orchestration and adapters
internal/tools                  read-only Tool/MCP gateway adapters
```

Rules enforced in review/CI:

- domain packages import only the standard library and domain siblings;
- provider SDK types never appear in public domain/application interfaces;
- interfaces are consumer-owned and behavior-focused, not one-for-one wrappers
  around vendor clients;
- constructors accept dependencies explicitly; no global client registry;
- every adapter has contract tests and fault-injection coverage;
- network calls never occur inside application database transactions.

## 11. Version, upgrade and provenance policy

- Pin Go modules; commit `go.mod` and `go.sum`; verify through the Go checksum
  database or an approved internal mirror.
- Pin container images by version and digest; generate an SBOM and provenance in
  release CI.
- Track current patch releases automatically, but merge only after unit,
  integration, migration, compatibility and security gates pass.
- PostgreSQL minor updates are routine; major updates run dump/restore or
  `pg_upgrade` rehearsal and the full supported-major suite.
- Valkey/OpenSearch minor/major upgrades use backup, compatibility, rolling-
  upgrade and rollback tests; do not assume protocol/API compatibility.
- Re-review licenses and notices for every direct dependency and distributed
  binary before release.

## 12. Decision matrix

Scores are 1 (poor) to 5 (strong) for the MVP requirements, not universal product
rankings.

| Choice | Correctness fit | Operability now | Scale headroom | Portability | MVP total /20 |
|---|---:|---:|---:|---:|---:|
| PostgreSQL final state | 5 | 4 | 4 | 5 | 18 |
| PostgreSQL lease queue | 5 | 5 | 3 | 4 | 17 |
| Valkey transient state | 4 | 4 | 5 | 4 | 17 |
| S3 Artifact contract | 5 | 4 | 5 | 4 | 18 |
| OpenSearch hybrid index | 4 | 3 | 5 | 4 | 16 |

Scores are re-evaluated at the exit triggers; they are not used to bypass
security, SLO or relevance evidence.

## 13. Consequences

### Positive

- One transactional source of truth keeps Session concurrency and final commit
  auditable.
- Reference versions are current and reproducible while supported compatibility
  windows remain practical.
- Queue, cache, object, search and MCP boundaries have concrete contract and exit
  tests rather than nominal interfaces.
- The open-source project does not require a proprietary cloud to run its core
  test suite.

### Costs and risks

- PostgreSQL carries final state, outbox and initial queue load; connection and
  polling budgets need careful enforcement.
- OpenSearch adds the largest operational footprint and must justify itself with
  relevance/scale measurements.
- Redis-protocol products are not assumed behaviorally or legally identical.
- Supporting PostgreSQL 16–18 and two Go families expands CI.

## 14. Official source snapshot

Version facts were rechecked on 2026-08-25: Go 1.27.0, PostgreSQL 18.6,
Valkey 9.1.1, and OpenSearch 3.8.0 were the official current reference releases.
Repository tests pin PostgreSQL 18.6 and Valkey 9.1.0. Automation must continue
to recheck them:

- Go release history and support policy: <https://go.dev/doc/devel/release>
- Go downloads and checksums: <https://go.dev/dl/>
- PostgreSQL versioning/support policy: <https://www.postgresql.org/support/versioning/>
- PostgreSQL `SKIP LOCKED`: <https://www.postgresql.org/docs/current/sql-select.html>
- Valkey releases: <https://valkey.io/download/>
- Valkey support policy: <https://valkey.io/topics/releases/>
- OpenSearch version history: <https://docs.opensearch.org/latest/version-history/>
- OpenSearch hybrid search: <https://docs.opensearch.org/latest/vector-search/ai-search/hybrid-search/>
- Amazon S3 API reference: <https://docs.aws.amazon.com/AmazonS3/latest/API/Welcome.html>

## 15. Acceptance checklist

- [x] Go, PostgreSQL, Redis-compatible transient state, queue, object storage and
  vector/hybrid search have concrete reference choices.
- [x] Each choice lists alternatives, constraints and an exit strategy.
- [x] Queue and search/vector dependencies are isolated behind repository-owned
  interfaces.
- [x] PostgreSQL is final truth; Valkey and queue delivery are not.
- [x] Knowledge MCP placement preserves trusted tenant/ACL injection.
- [x] Version/support, upgrade, security and provenance policy are explicit.
- [x] Selection is reconciled with P0 capacity, SLO and threat-model constraints.
