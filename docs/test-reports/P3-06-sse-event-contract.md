# P3-06 SSE event contract test report

- Date: 2026-08-23
- Commit under test: working tree before P3-06 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0028 SSE event contract](../requirements/0028-sse-event-contract.md)

## 1. Result

P3-06 passes all known-event schemas, strict/compatible decoding, public-field
privacy, duplicate-key rejection and concurrent Run-local sequence validation.
`internal/events` has 81.5% statement coverage.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./...` | Pass |
| Coverage | `go test -cover ./internal/events` | Pass; 81.5% |
| Repetition | `go test -count=100 ./internal/events` | Pass |
| Full Go 1.26 | `go test -count=1 ./...`; `go vet ./...` | Pass |
| Full Go 1.27 | `go test -count=1 ./...`; build | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |
| Race detector | `go test -race ./...` | Not executable locally: no C compiler/CGO; mandatory in Linux CI |

## 3. Type and schema evidence

Table tests construct and strict-round-trip all 13 initial types: queued,
started, Step/retrieval/tool lifecycle, assistant delta, cancellation and every
terminal Run outcome. Each encoded envelope has exactly seven contract keys,
uses schema version 1, produces decimal SSE ID from sequence and omits tenant.
Payload getters are defensive copies.

Wrong payload/type pairs, unknown emit types, invalid typed IDs, nonterminal Step
completion, invalid max steps, oversized/empty delta, unsafe reason text and
sequence beyond signed 64-bit storage range are rejected.

## 4. Compatibility and parser evidence

Strict decode accepts only current schema/known type and uses unknown-field
rejection for both envelope and typed payload. A token-walking preflight rejects
duplicate keys at any object depth, preventing last-key-wins ambiguity.
Noncanonical/non-UTC timestamps, tenant injection, raw-prompt fields and trailing
or malformed JSON fail closed.

Compatible decode accepts a new `citation.added` type and a future schema with an
additive envelope field. It validates core identity/sequence/time and preserves
payload bytes while marking the Event unknown. Strict decode rejects both, so
current emit/storage code cannot accidentally publish unsupported shapes.

## 5. Privacy and ordering evidence

Contract tests search emitted JSON for tenant, prompt, arguments, provider error
and chain-of-thought fields. Attempts to emit arbitrary maps with tool arguments
or provider error text fail because emission requires exact safe payload types.

SequenceTracker accepts a gap from 1 to 3, rejects duplicate 3 and backward 2,
and rejects a tenant/Turn identity change under the same Run. Forget explicitly
ends retained tracker state. One hundred concurrent independent Run streams are
accepted under the same tracker; Linux CI applies the race detector.

## 6. Acceptance conclusion

- Versioned envelope includes Turn/Run/sequence/time/type: **Pass**.
- Sequence is strictly monotonic per Run and gap-tolerant: **Pass**.
- Unknown types and future schemas have explicit compatibility behavior: **Pass**.
- Hidden reasoning/raw sensitive fields cannot be emitted by current types: **Pass**.
- Heartbeats remain outside durable Event/sequence types: **Pass by protocol boundary**.

P3-06 is complete; race execution remains enforced by Linux CI.
