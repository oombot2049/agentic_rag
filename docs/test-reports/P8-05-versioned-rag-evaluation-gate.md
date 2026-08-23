# P8-05 Versioned RAG Evaluation Test Report

- Result: Pass
- Date: 2026-08-23
- Go versions: 1.26.7 and 1.27.0
- Gate: `make eval`

## Baseline result

| Property | Result |
| --- | ---: |
| Dataset | `agenticrag_rag_v1` / `revision_0001` |
| SHA-256 | `0f7d774963b117ffad2b2a955b59c44376efcc3f84ea467a9c60819e0a6fd8a2` |
| Cases / human reviewed | 10 / 3 (30%) |
| Recall@5 | 1.000000 |
| MRR@5 | 1.000000 |
| Citation correctness F1 | 1.000000 |
| Claim-support faithfulness | 1.000000 |

The checked-in predictions are a deterministic reference contract. The result
does not claim that an external model achieves perfect production quality.

## Negative coverage

Tests prove that the gate rejects unknown and duplicate JSON fields, malformed
or duplicate identifiers, missing/extra cases, a human-review slice below 20%,
invalid baseline metrics, and a Golden Set digest mismatch. Metric tests cover
rank cutoff, missing relevant Chunks, reciprocal rank, invented/missing
citations, annotated contradictions, unsupported claims, and the vacuous answer
case.

A degraded prediction for one of ten cases produces stable sorted violations:

```text
faithfulness 0.900000 below minimum 0.950000
faithfulness 0.900000 below regression floor 0.950000
```

Prediction case order is reversed in a separate test and produces byte-equal
metric values, confirming macro aggregation is order independent.

## Quality gates

| Gate | Go 1.26.7 | Go 1.27.0 |
| --- | --- | --- |
| `go vet ./...` | Pass | Pass |
| `go test -count=1 ./...` | Pass | Pass |
| reference `rageval` CLI | Pass | Pass |
| `staticcheck ./...` | Pass | Not required |

The CI quality matrix runs `make eval` for both supported Go versions. The CLI
prints one machine-readable JSON report on success or quality failure, returns
non-zero for violations, and does not require network, credentials, database,
or wall-clock state.
