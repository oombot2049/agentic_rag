# Requirement P7-01: End-to-end OpenTelemetry Tracing

- Status: Complete
- Priority: P0
- Owner: Go Backend
- Last reviewed: 2026-08-23
- Depends on: P1-03 Structured Logging, P4-01 Run Queue
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P7-01

## 1. Objective

Add vendor-neutral OpenTelemetry traces across synchronous HTTP handling and
asynchronous Run execution, with spans for queue consumption, PostgreSQL,
Redis, RAG and LLM work. Export is owned by a lifecycle component with bounded
batching, parent-based ratio sampling, force-flush and graceful shutdown.

```mermaid
sequenceDiagram
  participant C as Client
  participant H as HTTP API
  participant Q as Durable Run Queue
  participant W as Agent Worker
  participant D as PostgreSQL/Redis
  participant R as RAG/LLM
  C->>H: W3C traceparent
  activate H
  Note over H: SERVER span; no body/query attributes
  H->>Q: persist current traceparent/tracestate
  deactivate H
  Q-->>W: delivery later
  W->>W: new CONSUMER Run trace
  Note over W: Span Link -> HTTP span context (not parent/child)
  W->>D: CLIENT spans; operation only, no SQL/key/args
  W->>R: INTERNAL spans; stage/provider/model and numeric usage only
```

## 2. Propagation and async causality

Only the W3C Trace Context propagator is enabled; baggage is intentionally not
accepted, preventing tenant or user-controlled data from becoming implicit
attributes. HTTP middleware extracts the remote context and starts a SERVER
span. Turn creation injects the current server span into the existing durable
queue trace fields, falling back to the already validated inbound headers only
when middleware is absent in isolated tests.

A Worker does not continue the potentially expired request trace as a child.
It starts a new root CONSUMER span and adds exactly one Link to the persisted
request SpanContext. Redelivery creates a new Run-processing span with the same
causal Link and a numeric delivery count.

## 3. Cardinality and sensitive-data policy

Allowed attributes are a closed set: HTTP method/status; messaging system and
operation; DB system and SQL operation verb; Redis command name; RAG stage,
degraded flag and counts; configured LLM provider/model plus token/cost counts.
No tenant/session/turn/Run IDs, URL path/query, SQL statement, Redis key/value,
retrieval query, prompt, Chunk content, Tool arguments, model output, API key or
provider request ID is emitted. IDs remain in correlated structured logs.

PostgreSQL instrumentation derives only an allowlisted first SQL verb. Redis
hooks use the library command name and never inspect arguments. Configured
provider/model values already pass bounded registries; client-controlled
operation keys are excluded.

## 4. Failure and lifecycle behavior

Application work never fails because an exporter fails. The SDK queues bounded
batches and reports exporter errors through its normal error handler. Shutdown
first force-flushes and then closes the provider under the application shutdown
deadline. Span status is ERROR and records the normalized error type; raw
request/response data remains absent.

## 5. Test strategy

An in-memory exporter integration test sends a remote HTTP context, injects it
into a queue message, starts a Worker-style Run span and emits DB/Redis/RAG/LLM
children. It asserts the request and Run use different Trace IDs, the Run has a
Link to the request span, child parenting is correct and exported attributes
contain none of a sensitive/high-cardinality sentinel set. Unit tests cover
sampling bounds, lifecycle idempotency, malformed/empty queue context, HTTP
status/error recording, SQL verb normalization and Redis pipeline spans.

## 6. Acceptance checklist

- [x] HTTP and asynchronous Run traces are connected by Span Link.
- [x] PostgreSQL, Redis, RAG and LLM operations emit bounded spans.
- [x] W3C context survives durable queue serialization.
- [x] Exporter flush/shutdown is lifecycle managed.
- [x] Sensitive and high-cardinality values are absent from span data.

