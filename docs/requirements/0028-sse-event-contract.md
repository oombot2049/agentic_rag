# Requirement P3-06: Versioned SSE event contract

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P0-05 domain/API contract
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-06

## 1. Objective and envelope

Define the durable public events later replayed by
`GET /v1/runs/{run_id}/events`. An event contains schema version, type, Turn ID,
Run ID, positive Run-local sequence, UTC occurrence time and an object payload.
Tenant identity is retained internally for storage/authorization but is excluded
from public JSON. The SSE `id` is the decimal sequence and the SSE `event` field
is the event type.

```json
{
  "schema_version": 1,
  "type": "assistant.delta",
  "turn_id": "trn_...",
  "run_id": "run_...",
  "sequence": 42,
  "occurred_at": "2026-08-23T00:00:03Z",
  "payload": {"text":"Retention"}
}
```

## 2. Ordering and delivery semantics

Sequence is positive, signed-64-bit-safe, strictly increasing per Run and never
reused. It need not be gap-free: a rolled-back allocator or retention policy may
leave gaps. A stream tracker rejects duplicate/backward sequence and any Turn or
tenant mismatch for a Run. Transport delivery remains at-least-once; clients
deduplicate by `(run_id, sequence)`. `Last-Event-ID=N` means replay strictly
after N.

Heartbeat comments are transport liveness, not Events. They have no ID, do not
consume sequence and are never persisted. A terminal event may be appended only
after its matching terminal state transaction commits; the protocol type alone
does not authorize early publication.

## 3. Initial event types and safe payloads

| Type | Public payload |
|---|---|
| `run.queued` | normalized `max_steps` |
| `run.started` | empty object |
| `step.started` | Step ID/no and public kind |
| `retrieval.completed` | Step ID and bounded result count |
| `tool.started` | Step ID, Tool Call ID and registered tool name |
| `tool.completed` | Step/Tool Call IDs and public success flag |
| `assistant.delta` | bounded user-visible text delta |
| `step.completed` | Step ID/no and public status |
| `run.cancel_requested` | empty object |
| `run.completed` | selected final Message ID |
| `run.failed` | stable safe reason code only |
| `run.cancelled` | empty object |
| `run.timed_out` | stable safe reason code only |

Emission accepts only these strong payload types, rejects unknown JSON fields,
validates typed IDs/bounds and copies payload bytes. There is no field for raw
prompt/chunk, tool arguments/result, provider error, credential, internal plan or
chain-of-thought. Large public results use authorized Artifact references in a
later additive schema rather than embedding bytes.

## 4. Compatibility strategy

Schema version 1 permits additive optional payload fields after review. Existing
field meaning, type and requiredness cannot change. New event types are additive
and clients must ignore types they do not understand.

Strict server decode accepts only schema 1 and known types/payloads, protecting
durable storage and emission. Compatible client/gateway decode validates the
envelope but preserves unknown types or future schema payload bytes and marks
them unknown; it never treats them as terminal. Non-canonical/trailing JSON,
duplicate semantic fields, invalid IDs/type names, oversized payloads and invalid
time/sequence fail closed.

## 5. Test strategy and acceptance

- table tests round-trip every event type and reject wrong payload/type pairs;
- contract tests compare exact JSON keys and guarantee tenant/private fields are absent;
- stream tests cover gaps, duplicates, backward sequence, Run/Turn/tenant mixing
  and concurrent acceptance;
- compatibility tests preserve unknown type/future version while strict decode rejects;
- privacy tests attempt forbidden/raw fields and oversized deltas.

- [x] Every durable event includes Turn, Run and a positive monotonic sequence.
- [x] Schema version and unknown-event evolution rules are explicit.
- [x] Current emitters cannot serialize hidden reasoning or raw sensitive fields.
- [x] Heartbeats are outside the durable sequence.
