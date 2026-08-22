# ADR-0001: Adopt a linear, durable Agentic RAG MVP baseline

- Status: Accepted
- Date: 2026-08-23
- Decision owners: Architecture and backend maintainers
- Requirement: [P0-01 MVP scope](../requirements/0001-mvp-scope.md)

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
4. Commands use HTTP/JSON. Server-to-client progress uses resumable GET SSE.
5. Agent execution is a bounded Run → Step → Attempt loop.
6. PostgreSQL is the final source of truth; queue, cache and event stream state
   is reconstructable.
7. Retrieval and tool/provider integrations sit behind Go interfaces. A
   knowledge base may participate as an MCP tool through a retrieval adapter.
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

Rejected. MCP is an integration transport. A repository-owned Retriever and
Tool contract protects domain code from protocol and vendor churn.

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
- SSE requires a durable event cursor and slow-consumer policy.

### Follow-up decisions

P0-02 must quantify capacity assumptions; P0-03 must set SLO/error budgets;
P0-04 owns the threat model; P0-05 freezes the domain/API vocabulary; P0-06
selects concrete infrastructure with exit strategies.
