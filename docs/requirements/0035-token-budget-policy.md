# Requirement P4-05: Model-Exact Token Budget and Explainable Truncation

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P4-04 immutable Context Snapshot
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P4-05

## 1. Safety invariant

For an exact model version, the Budgeter must either return a prompt plan whose
declared input plus reserved output and safety margin does not exceed the model
window, or return an error and make no model call.

```text
input_tokens + reserved_output_tokens + safety_margin_tokens <= window_tokens
```

No character/rune heuristic is accepted as a production fallback. A Registry
selects a tokenizer by exact normalized model ID; an unknown model fails closed.

## 2. Retention and truncation policy

System instructions are mandatory and never silently truncated. The current
user message is mandatory but may be tokenizer-truncated only when it is the
remaining cause of overflow; that decision is explicit. Optional content is
admitted in this order:

1. completed history, newest Turn first and only as whole user/assistant pairs;
2. curated memory in supplied order;
3. RAG evidence in retrieval rank order.

Returned entries retain canonical prompt order even though admission evaluates
recent history first. Optional entries that do not fit are dropped; a final
optional memory/RAG entry may be tokenizer-truncated to consume useful residual
budget. History pairs are never split or partially truncated.

Every entry includes configured framing overhead in addition to content tokens.
This accounts for role/message delimiters without embedding provider syntax in
the Context Builder.

## 3. Explainability and immutability

The immutable Plan records exact model, window, input limit, used tokens,
reserved output, safety margin, selected entries and one decision per original
entry: kept, dropped or truncated with original/final token counts and a stable
reason code. Token counts are recomputed after tokenizer truncation and the
invariant is checked again before return.

## 4. Test strategy

Boundary tests cover zero residual input, mandatory overflow, exact fit, one
token overflow, model lookup, tokenizer errors and truncation that does not obey
its requested limit. Deterministic randomized/property tests generate Context
Snapshots, windows and token distributions and require every successful Plan to
stay within its window, preserve whole history pairs and never mutate inputs.

## 5. Acceptance checklist

- [x] Successful Plans can never exceed the exact model window.
- [x] Output reservation and framing/safety overhead are always charged.
- [x] System policy is never silently dropped or truncated.
- [x] Recent history pairs outrank memory and RAG and remain structurally whole.
- [x] Every exclusion/truncation is deterministic and explainable.
- [x] Unknown model/tokenizer behavior fails closed.
