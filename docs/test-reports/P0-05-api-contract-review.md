# P0-05 validation report: domain and API contract

- Result: Passed
- Date: 2026-08-23
- Validation type: OpenAPI and state-machine review

## Contract coverage

| Area | Result | Evidence |
|---|---|---|
| Identifier meanings | Pass | Contract §2 |
| Turn with multiple Runs | Pass | Contract §3.2 |
| ToolCall/RetrievalResult/Artifact | Pass | Contract §3.3 |
| Request/trace/idempotency IDs | Pass | Contract §2.1 and §5 |
| Context/row version conflict | Pass | Contract §6 |
| State transition legality | Pass | Contract §7 |
| Main-chain transaction boundaries | Pass | Contract §8 |
| POST Turn and GET SSE | Pass | Contract §4, §9 and OpenAPI |
| Cancellation and regeneration | Pass | Contract §11 and OpenAPI |
| Stable error model | Pass | Contract §10 and OpenAPI |
| Branch excluded | Pass | Contract §1, §6 and §14 |

## Machine-readable checks

- `openapi-v1.json` parses as strict JSON.
- OpenAPI version is 3.1.0.
- Nine public path templates are present.
- Every command operation declares the required `Idempotency-Key` parameter.
- Create Turn requires `expected_context_version` and `message`.
- SSE declares `text/event-stream` and optional `Last-Event-ID`.
- Session, Turn, Run, Message and Error schemas are present.
- All path parameters are declared required.
- Error responses reference the common stable error envelope.

## State-machine review

- A Step retry creates an Attempt, while a full retry/regeneration creates a Run.
- A Turn may own multiple Runs but only one Run can be selected.
- Final commit requires valid worker fencing and Session ownership/version.
- Attempt `UNKNOWN` cannot be blindly retried for unsafe side effects.
- Assistant deltas are durable events; only selected final content becomes a
  normal conversation Message.

## Conclusion

P0-05 freezes object boundaries, statuses, idempotency and version-conflict
semantics consistently across the narrative contract and OpenAPI draft.
