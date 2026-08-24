# ADR-0001: Adopt a linear, durable Agentic RAG architecture

- Status: Accepted
- Date: 2026-08-23
- Decision owners: Architecture and backend maintainers

## Context

An online multi-tenant Agentic RAG service combines long-running execution,
streaming delivery, external providers and mutable conversation state. Leaving
conversation topology, concurrency or durability implicit would allow different
packages to implement incompatible semantics.

The first open-source release needs one coherent model that can be tested under
failure. It also needs extension seams for knowledge MCP, retrieval backends,
queues and providers without making their protocols domain concepts.

## Decision

The MVP adopts all of the following as one architecture baseline:

1. Conversations are linear Sessions; Branch is not modeled.
2. A Session has at most one Active Run. A busy request is rejected rather than
   queued until measurements justify another policy.
3. A Turn is the logical question and may own multiple Runs; only one Run result
   can become the committed assistant Message.
4. Commands use HTTP/JSON. Durable lifecycle progress uses resumable `/events`
   SSE; provisional assistant text uses the Redis-only `/output` SSE.
5. Agent execution is a bounded Run → Step → Attempt loop.
6. PostgreSQL is the final source of truth; queue, cache and lifecycle live-tail
   state is reconstructable. Transient assistant output may expire and is
   replaced by the final Message rather than reconstructed delta by delta.
7. Retrieval and provider integrations sit behind Go interfaces. The only MVP
   tool is read-only `knowledge.search`; a knowledge base may participate through
   a direct or MCP retrieval adapter. Mutating tools require a future ADR.
8. Large immutable execution data is stored as Artifacts outside hot relational
   rows, with hashes and tenant-scoped references in PostgreSQL.

## Alternatives considered

### Branching conversation graph in the first release

Rejected. It multiplies context-head, conflict, selection, retention and API
semantics before the linear path is proven. A future ADR may add it without
overloading Session or Turn identifiers.

### Multiple Active Runs per Session

Rejected for MVP. It requires deterministic merge or selection semantics and
raises the probability of lost context updates. Multiple candidate Runs remain
possible under one Turn, but do not concurrently mutate the Session head.

### WebSocket as the only client protocol

Rejected. The request path is naturally a command and SSE provides reconnectable
one-way progress using standard HTTP infrastructure. WebSocket remains an option
only if a later bidirectional use case cannot fit commands plus SSE.

### Redis or the queue as final execution state

Rejected. Their operational value does not justify losing durable audit and read
availability when an accelerator is unavailable.

### MCP as the core domain abstraction

Rejected. MCP is an integration transport. Repository-owned Retriever and
read-only Tool contracts protect domain code from protocol and vendor churn.

## Consequences

### Positive

- Concurrency and retry behavior are deterministic enough for automated tests.
- Client reconnect and worker recovery do not redefine the logical question.
- Vendor integrations are replaceable and tenant policy stays server-controlled.
- The committed conversation remains readable during Redis/event-stream faults.

### Costs

- Busy same-Session requests receive a conflict instead of being automatically
  queued.
- PostgreSQL transactions, leases, outbox delivery and Artifact lifecycle add
  implementation work.
- `/events` requires a durable event cursor and slow-consumer policy. `/output`
  uses an independent short-lived Redis cursor and recovers gaps to the final
  Message rather than promising replay of every delta.

### Follow-up decisions

Capacity assumptions, SLO and error budgets, the threat model, the domain/API
contract, and infrastructure exit strategies are maintained as separate,
reviewed architecture documents.
