# AgenticRAG

A durable, multi-tenant Agentic RAG backend written in Go.

AgenticRAG turns long-running LLM, retrieval, and read-only knowledge workflows into recoverable executions. PostgreSQL stores authoritative business and execution state; transactional outbox delivery bridges asynchronous systems; fenced leases prevent stale workers from writing; durable lifecycle SSE combines PostgreSQL replay with a Redis live tail; and Redis-only output streams provisional assistant text without adding PostgreSQL rows.

**Durable Agent Runtime · Transactional Outbox · Durable Lifecycle SSE · Transient Output · Tenant-safe RAG**

## Why AgenticRAG?

A production Agentic RAG request is not a single model call. It may execute multiple planning, retrieval, generation, and read-only knowledge-tool steps across unreliable external providers. The execution can outlive an HTTP connection or a worker process.

This project focuses on the backend guarantees required by that workflow:

- recoverable Agent execution with durable checkpoints;
- atomic Turn, Run, event, and dispatch state;
- at-least-once delivery with explicit idempotency;
- lease takeover protected by monotonic fencing tokens;
- resumable lifecycle events plus final-result recovery during client or Redis failures;
- authorization-first retrieval through a read-only knowledge tool;
- bounded step, time, token, provider, tool, and cost budgets.

## Architecture

```mermaid
flowchart TB
    Client[API Client]
    Serve[serve<br/>HTTP API and SSE]
    PG[(PostgreSQL<br/>Authoritative State)]
    Worker[worker<br/>Agent Runtime]
    Providers[Retrieval, LLM, and read-only Knowledge Tool]

    Client <-->|HTTP and SSE| Serve
    Serve <-->|commands and replay| PG
    PG -->|Run Queue work| Worker
    Worker -->|execute steps| Providers
    Worker -->|checkpoint and commit| PG

    PG -->|queue and lifecycle Outbox| Relay[relay<br/>Outbox delivery]
    Relay -->|durable lifecycle tail| Redis[(Redis / Valkey<br/>Transient Streams)]
    Worker -->|batched assistant deltas| Redis
    Redis -.->|events and output SSE| Serve

    Sources[Knowledge Sources]
    Sources --> Ingest[ingest-worker]
    Ingest --> Knowledge[(Artifacts and Search Index)]
    Knowledge --> Providers
```

The authoritative execution path is `Client -> serve -> PostgreSQL -> worker -> providers`.
Outbox delivery and knowledge ingestion are supporting paths. PostgreSQL remains
the system of record; Redis and the search index can be rebuilt or bypassed
without losing committed conversation state.

The single binary exposes four independently scalable process roles:

| Role | Responsibility | Scaling signal | Status |
|---|---|---|---|
| `serve` | HTTP commands, queries, authentication, and the SSE Gateway | request rate and SSE connections | composed |
| `relay` | Outbox discovery, routing, retry, and publication | Outbox backlog | composed |
| `worker` | Run Queue consumption and recoverable Agent execution | active Runs and provider quotas | lifecycle ready; provider adapters required |
| `ingest-worker` | parsing, chunking, embedding, and index updates | document and embedding throughput | lifecycle ready; source/provider adapters required |

PostgreSQL is the final source of truth. Redis streams, caches, queue projections, search indexes, and artifacts are either transient or reconstructable from durable identities and metadata.

## Core execution flow

```mermaid
sequenceDiagram
    participant C as Client
    participant A as serve
    participant P as PostgreSQL
    participant R as relay
    participant W as worker
    participant X as LLM / Retrieval / Tool
    participant V as Redis

    C->>A: POST Turn + Idempotency-Key
    A->>P: TX: reserve Turn/Run + RunQueued Outbox
    A-->>C: TurnID + RunID
    R->>P: claim RunQueued Outbox
    R->>P: enqueue durable Run delivery + ACK
    W->>P: claim delivery + acquire fenced Run lease
    loop bounded Agent steps
        W->>P: persist checkpoint / Step / Attempt
        W->>X: execute external operation
        W->>P: TX: append lifecycle event + Outbox
        R->>P: claim next lifecycle sequence
        R->>V: idempotent lifecycle append
        W->>V: batched assistant.delta (provisional)
        V-->>A: lifecycle / output streams
        A-->>C: /events and /output SSE
    end
    W->>P: TX: final Message + Session CAS + terminal event
    R->>V: publish terminal lifecycle event
    V-->>A: run.completed
    A-->>C: authoritative completion
```

Durable business transitions never publish directly to Redis or a queue. They commit an Outbox record with the business state, and Relay publishes that intent later. Provisional `assistant.delta` output is the explicit exception: it goes directly to a short-lived Redis Stream and is never authoritative.

## Reliability model

### PostgreSQL as the authority

Session, Message, Turn, Run, Step, Attempt, queue delivery, lease, event, Outbox, idempotency, and audit state are durable in PostgreSQL. The committed conversation remains readable if Redis or a downstream provider is unavailable.

### At-least-once plus idempotency

The system does not claim end-to-end exactly-once delivery. Queue and Outbox items may be delivered more than once. Duplicate effects are controlled with:

- command `Idempotency-Key` and request fingerprints;
- stable queue delivery and tool invocation identities;
- deterministic SSE stream IDs;
- idempotent Redis Lua publication;
- version CAS and uniqueness constraints during final commit.

### Lease plus fencing

A lease expiry alone cannot prevent split brain: a paused worker may resume after another worker has taken over. Every takeover advances a fencing token or execution version. Critical mutations include that authority, so PostgreSQL rejects writes from stale workers.

### Atomic final commit

The final transaction validates Run authority, inserts the assistant Message, selects the winning Run, advances the Session context version, and appends the terminal event with its Outbox intent. Partial visibility is not allowed.

### Durable lifecycle plus transient output

Lifecycle events receive monotonically increasing PostgreSQL sequences. The `/events` SSE endpoint replays missed lifecycle events using `Last-Event-ID`, then follows Redis for low latency. Periodic PostgreSQL high-water checks close Redis gaps; PostgreSQL polling replaces Redis when it is unavailable.

Assistant text deltas are presentation data, not durable business events. The Worker-side batcher combines provider fragments until 512 bytes, 75 ms, or an explicit boundary, then publishes them to the Redis-only `/output` stream. That stream has its own short-lived cursor and never writes `run_events` or Outbox rows. If Redis data is unavailable or the output cursor expires, clients read the current Run and the authoritative final Message instead of replaying every animation frame.

## Supported capabilities

### Durable Agent runtime

- Run → Step → Attempt execution model;
- PostgreSQL-backed checkpoints and worker takeover;
- strict structured action protocol;
- bounded steps, retries, wall time, tokens, provider calls, tool calls, and estimated cost;
- LLM provider routing, response validation, error classification, and circuit breaking;
- atomic final answer commit.

### RAG and knowledge ingestion

- bounded document parsing and normalized chunk construction;
- recoverable batch embedding pipeline;
- versioned search-index port;
- grounded query rewriting;
- authorization-first hybrid retrieval;
- token-bounded reranking;
- immutable retrieval evidence and audit records.

### Reliable asynchronous execution

- transactional Outbox;
- PostgreSQL Run Queue with `FOR UPDATE SKIP LOCKED`;
- global and per-tenant worker concurrency limits;
- renewable queue and Run leases with fencing;
- bounded exponential retry and terminal `DEAD` Outbox state;
- graceful worker drain at durable safe points.

### Durable lifecycle and transient output delivery

- versioned lifecycle SSE contract on `/events`;
- monotonic durable per-Run sequence and paginated PostgreSQL replay;
- idempotent Redis/Valkey lifecycle live tail;
- Redis-only `assistant.delta` stream on `/output` with an independent cursor;
- slow-consumer protection and endpoint-specific reconnect;
- PostgreSQL fallback for lifecycle events and final-Message fallback for output.

### Multi-tenant security and tool governance

- trusted tenant Principal from server-side authentication;
- PostgreSQL API keys stored as SHA-256 digests;
- tenant-scoped repositories, events, queue messages, Redis keys, and retrieval filters;
- deny-by-default, read-only tool registry with schema and capability validation;
- knowledge access ledger with duplicate-call protection;
- immutable tool audit records;
- sensitive-data redaction and auditable retention/deletion workflows.

### Operations and quality

- structured context-aware logging;
- OpenTelemetry trace helpers and bounded-cardinality Prometheus metric types;
- hierarchical tenant quotas;
- checked-in operational/business dashboards, alert rules, and runbooks;
- unit, integration, end-to-end, concurrency, evaluation, and load-contract suites.

The observability packages and deployment artifacts are implemented, but the
current `serve` composition does not yet mount `/metrics` or install HTTP
metrics/tracing middleware. Operators must not assume the checked-in dashboards
are populated until deployment composition wires those exporters and recorders.

## Failure behavior

| Failure | Behavior | Correctness impact |
|---|---|---|
| Redis unavailable | lifecycle SSE switches to PostgreSQL polling; transient output pauses | durable events and final Message remain available |
| Relay crashes after publish | the claim expires and publication is retried | downstream idempotency absorbs duplicates |
| Worker loses its lease | another worker resumes from the checkpoint | stale writes fail fencing checks |
| SSE client disconnects | the Run continues; the client reconnects with that endpoint's cursor | no new Turn or Run is created; expired output falls forward to the final Message |
| LLM timeout or throttling | the Gateway classifies, backs off, or opens the circuit within the Run budget | the Run retries or fails explicitly |
| Final commit conflicts | the complete transaction rolls back | no partially visible assistant Message |
| Search provider fails | retrieval fails explicitly and records no fabricated evidence | availability drops without weakening authorization |

## Domain invariants

- A Session is linear and has at most one Active Run.
- A Turn is one logical question and may own multiple Runs.
- Only one Run result can become the committed assistant Message for a Turn.
- A Step is a logical action; an Attempt is a technical retry of that action.
- External provider calls are never wrapped in a database transaction.
- Every authoritative takeover mutation carries fencing authority.
- Tenant scope comes from the authenticated Principal, never from model output.
- Retrieval applies tenant and ACL filters before ranking.
- LLM, retrieval, MCP, and tool outputs are treated as untrusted data.
- Redis and cache availability are not correctness requirements.

The MVP intentionally excludes branching conversation graphs and concurrent Runs mutating the same Session. These features require explicit head selection and merge semantics rather than an implicit extension of the linear model.

## Repository layout

```text
cmd/agenticrag/            executable and process-role dispatch
internal/agent/            recoverable Agent state machine and checkpoints
internal/domain/           Session, Turn, Run, Step, Attempt, and Message models
internal/repository/       PostgreSQL application repositories and final commit
internal/outbox/           reliable event claims, routing, retry, and relay loop
internal/queue/            PostgreSQL Run Queue
internal/runlease/         execution lease renewal and fencing
internal/transport/http/   command and query APIs
internal/transport/sse/    durable lifecycle replay and Redis-only output
internal/ingestion/        document parsing and ingestion pipeline
internal/embedding/        recoverable embedding batches
internal/retrieval/        rewrite, hybrid retrieval, and audit
internal/tools/            registry, execution boundary, and call ledger
internal/process/          production composition roots
migrations/                embedded PostgreSQL migrations
tests/                     integration, E2E, concurrency, and load suites
evals/                     versioned RAG quality evaluation
deploy/                    dashboards, alerts, and runbooks
docs/                      architecture, API, security, capacity, and SLO
```

## Build and verification

### Prerequisites

- Go 1.26 or newer; Go 1.27 is the preferred development toolchain.
- GNU Make for the canonical convenience targets.
- PostgreSQL for integration and process execution.
- Redis/Valkey for lifecycle live tail, transient output, and the Relay process.

```bash
git clone https://github.com/oombot2049/agentic_rag.git
cd agentic_rag

go test ./...
go vet ./...
make build
./bin/agenticrag version
```

Additional verification targets:

```bash
make integration
make e2e
make concurrency
make eval
make load-contract
```

Integration suites provision pinned dependencies where required. PostgreSQL migrations are embedded from `migrations/` and must be applied before starting a service role against a new database.

## Running composed roles

Development `serve` requires an explicit tenant-scoped credential and cursor-signing key:

```bash
export AGENTICRAG_ENVIRONMENT=development
export AGENTICRAG_POSTGRES_DSN='postgres://agenticrag:secret@127.0.0.1:5432/agenticrag?sslmode=disable'
export AGENTICRAG_REDIS_ADDRESS='127.0.0.1:6379'
export AGENTICRAG_DEV_TENANT_ID='ten_localdev'
export AGENTICRAG_DEV_BEARER_TOKEN='replace-with-at-least-16-bytes'
export AGENTICRAG_CURSOR_SIGNING_KEY='replace-with-at-least-32-random-bytes'

./bin/agenticrag serve
```

Start the Outbox Relay independently:

```bash
./bin/agenticrag relay
```

Production `serve` rejects development static credentials and resolves `X-API-Key` credentials from PostgreSQL. Only SHA-256 key digests are stored in `api_keys`.

`worker` and `ingest-worker` expose production lifecycle composition APIs but intentionally fail closed from the CLI until deployment-specific LLM/retrieval and artifact/embedding adapters are supplied. They do not start inert consumers that repeatedly claim and Nack durable work.

## Configuration

Configuration is read from validated `AGENTICRAG_*` environment variables. Important groups include:

| Group | Examples |
|---|---|
| Runtime | `AGENTICRAG_ENVIRONMENT`, `AGENTICRAG_LOG_LEVEL` |
| HTTP | `AGENTICRAG_SERVER_ADDRESS`, timeout and shutdown settings |
| PostgreSQL | `AGENTICRAG_POSTGRES_DSN`, pool, connection, and health timeouts |
| Redis | address, credentials, pool/timeouts, `AGENTICRAG_REDIS_MAX_TTL`, and one-hour default `AGENTICRAG_REDIS_OUTPUT_TTL` |
| Queue | per-tenant and global pending limits |
| Worker | ID, global/per-tenant concurrency, poll, lease, renew, and retry intervals |
| Security | cursor-signing key and environment-specific authentication settings |

Invalid, missing, whitespace-padded, or unsafe values fail startup validation. Secrets are wrapped by the configuration layer and are not included in formatted output.

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [Core module design](docs/architecture/core-modules.md)
- [Architecture decisions](docs/adr/0001-linear-durable-architecture.md)
- [Technology selection](docs/adr/0002-technology-selection.md)
- [Domain and API contract](docs/api/domain-and-api-contract.md)
- [OpenAPI v1](docs/api/openapi-v1.json)
- [Capacity baseline](docs/capacity/workload-baseline.md)
- [Threat model](docs/security/threat-model.md)
- [SLO and error budgets](docs/slo/service-level-objectives.md)

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) for development expectations and [SECURITY.md](SECURITY.md) for vulnerability reporting. The project follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
