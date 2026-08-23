# Requirement P6-06: Built-in Tool Examples

- Status: Complete
- Priority: P1
- Owner: Go Backend
- Last reviewed: 2026-08-23
- Depends on: P6-02 Tool Call Ledger, P6-03 Tool Execution Boundary
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P6-06

## 1. Objective

Provide two safe, executable reference Tools that demonstrate both sides of the
Tool Gateway contract: `knowledge.search@1.0.0` is a Run-scoped read-only query;
`support.ticket.create@1.0.0` is a Run-scoped idempotent write. Both are normal
Registry definitions and Executor adapters, so examples cannot bypass schema,
permission, scope, timeout, concurrency, output-size or ledger controls.

```mermaid
sequenceDiagram
  participant A as Agent Worker
  participant R as Tool Registry
  participant L as Tool Ledger
  participant E as Bounded Executor
  participant B as Built-in Adapter
  participant P as Injected Port
  A->>R: Resolve(name/version/scope/permissions/arguments)
  R-->>A: sealed Invocation
  A->>L: Begin(Tool Call + Attempt)
  L-->>A: EXECUTE + stable invocation_key
  A->>E: Execute(Invocation, invocation_key)
  E->>B: validated AdapterRequest
  B->>P: tenant/run-scoped request
  P-->>B: bounded typed result
  B-->>E: JSON object
  E-->>A: schema-validated output
  A->>L: RecordOutcome
  Note over A,L: retry returns ledger result; no duplicate side effect
```

## 2. Contracts

Knowledge search requires permission `knowledge:search`, accepts a trimmed
query plus optional `top_k` (default 5, maximum 20), and returns bounded Chunk
references, content and finite scores. Its injected `KnowledgeSearcher` port
receives the exact tenant/session/Run scope and has no mutation method.

Ticket creation requires `support:ticket:create`, accepts a title, description
and closed priority enum, and returns a stable ticket ID/status. The adapter
must pass the ledger-derived `invocation_key` unchanged to `TicketCreator` as
the business idempotency key. Implementations must atomically map one key to
one request/result; same-key/different-request is a conflict. Retrying a
terminal Tool Call is served from the Tool Ledger before adapter execution.

## 3. Security boundary

Adapters have `NetworkNone` and execute only injected in-process ports. The
package contains no HTTP client, command execution, credential field, endpoint
or real production account. A production remote adapter is a separate type and
must declare brokered destinations so Executor policy can authorize them.
Both adapters defensively validate identity, scope, arguments and port output
even though the Registry also validates JSON Schema.

## 4. Test strategy

Contract tests verify Definition names, versions, permissions, effects, scope,
closed schemas and adapter capabilities. End-to-end tests execute both Tools
through Registry and Executor. The ticket test additionally uses the real
in-memory ledger to prove stable invocation-key propagation, terminal replay
and exactly one creator call. Negative tests cover malformed input, wrong
identity/scope, backend failure and invalid/unbounded backend output.

## 5. Acceptance checklist

- [x] Read-only query has no mutation port and is tenant/Run scoped.
- [x] Ticket creation propagates the durable business idempotency key.
- [x] Both examples traverse Registry, Executor and output validation.
- [x] Permissions, schemas, effects and capabilities are explicit.
- [x] No real account, credential or unrestricted network path is included.

