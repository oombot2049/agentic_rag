# AgenticRAG Documentation

This documentation describes the system architecture, reliability model, public contracts, operational envelope, and security boundaries of AgenticRAG.

## Recommended reading order

1. [Architecture overview](architecture/overview.md) — system boundaries, process topology, execution flow, and consistency model.
2. [Core module design](architecture/core-modules.md) — module responsibilities, mechanisms, failure handling, and extension points.
3. [Agent execution modes](architecture/agent-execution-modes.md) — deterministic RAG, bounded ReAct, and Checkpoint recovery.
4. [Technology selection](adr/0002-technology-selection.md) — infrastructure choices and migration triggers.
5. [Domain and API contract](api/domain-and-api-contract.md) and [OpenAPI v1](api/openapi-v1.json).

## Documentation map

| Directory | Content | Purpose |
|---|---|---|
| `architecture/` | System overview and core modules | Runtime design and reliability analysis |
| `adr/` | Accepted architecture decisions | Rationale, alternatives, and consequences |
| `api/` | Domain and protocol contracts | Identity, idempotency, errors, and SSE semantics |
| `capacity/` | Workload assumptions | Capacity planning and architecture limits |
| `security/` | Multi-tenant threat model | Trust boundaries and security controls |
| `slo/` | Service objectives and error budgets | Availability, latency, and degradation policy |

## System summary

AgenticRAG is a recoverable, multi-tenant Agentic RAG backend with PostgreSQL as its final source of truth. A question becomes a durable Run that can be leased, checkpointed, fenced, resumed, and audited. Transactional Outbox records decouple business commits from asynchronous publication. `/events` combines PostgreSQL lifecycle replay with a Redis live tail; `/output` carries short-lived Redis-only assistant deltas and falls back to the final Message.

## Implementation boundaries

- Implemented: domain state machines, transactional repositories, idempotency, PostgreSQL Run Queue, Worker framework, Run Lease, Agent checkpoints, retrieval and embedding pipelines, tool governance, SSE Gateway, Outbox Relay, observability primitives/deployment artifacts, and verification suites.
- Fully composed process roles: `serve` and `relay`.
- Lifecycle composition available, deployment adapters required: `worker` and `ingest-worker`.
- The repository does not include a production LLM vendor HTTP adapter or a complete external knowledge-source task adapter.
- `serve` currently exposes `/healthz` but does not mount `/metrics` or install
  HTTP tracing/metrics middleware; checked-in dashboards and alerts require
  deployment-side exporter/recorder wiring.
- Explicit Run regeneration under an existing Turn is designed but is not yet a
  mounted HTTP route; OpenAPI intentionally lists only composed endpoints.
- The composed MVP exposes only the read-only `knowledge.search` tool. Runtime
  human approval and mutating/destructive tool adapters are intentionally absent.
- Delivery semantics are at-least-once with idempotency and fencing; the project does not claim end-to-end exactly-once execution.
