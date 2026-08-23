# Requirement P8-05: Versioned RAG Evaluation Gate

- Status: Complete
- Priority: P0
- Owner: Algorithm / QA
- Last reviewed: 2026-08-23
- Depends on: P5-08 Retrieval audit
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P8-05

## 1. Objective

Provide a deterministic, versioned offline gate for retrieval and grounding
changes. A strategy change must be evaluated against the same immutable case
set, metric definitions, baseline digest, absolute floors, and permitted
regression budget.

```mermaid
flowchart LR
  G[Golden Set v1] --> V[Strict loader + semantic validation]
  P[Strategy predictions] --> V
  V --> M[Recall@K / MRR@K]
  V --> C[Citation set F1]
  V --> F[Claim-support faithfulness]
  B[Digest-bound baseline] --> Gate[Absolute + regression gate]
  M & C & F --> Gate
  Gate --> R[Machine-readable report / exit status]
```

## 2. Versioned data contracts

The Golden Set is JSON with `schema_version`, `dataset_id`, `revision`, `top_k`,
and cases. Every case contains a stable ID, query, relevant Chunk IDs, expected
Artifact citations, supported claims, contradiction markers, provenance, and a
human-review flag. Claims contain one or more answer phrases and the Artifact
IDs allowed to support that claim.

Predictions are a separate versioned file keyed by case ID. They contain ranked
Chunk IDs, cited Artifact IDs, and the final answer. Loaders reject unknown
fields, trailing JSON, duplicate IDs, malformed prefixes, missing/extra cases,
non-canonical ordering inputs, or empty claims. Well-formed but unexpected
citations remain valid predictions so the metric can penalize hallucinations.

At least 20% of Golden cases must be explicitly human reviewed and carry a
review note. This invariant prevents an LLM Judge from becoming the only source
of truth. Judge scores may be reported by future adapters, but they do not
replace the deterministic release metrics.

## 3. Metric definitions

For case `q` and cutoff `K`:

- `Recall@K(q) = |relevant(q) ∩ topK(q)| / |relevant(q)|`;
- `MRR@K(q) = 1 / rank(first relevant)` or zero if absent;
- citation correctness is set F1 between expected and emitted Artifact IDs,
  penalizing both invented and missing citations;
- faithfulness detects each annotated claim phrase in the answer and counts it
  supported only when an emitted citation intersects that claim's allowed
  support set. Annotated contradictions count as unsupported assertions. An
  answer with no detected claim scores zero rather than receiving a vacuous
  pass.

Suite metrics are macro averages, so high-volume/easy queries cannot hide a
weak case category. The report also includes case count and human-reviewed case
count.

## 4. Baseline and regression policy

The baseline records its schema version, Golden Set SHA-256, `top_k`, reference
metrics, absolute minimums, and maximum allowed drops. Evaluation fails closed
when the dataset digest or cutoff differs. Every metric must satisfy both:

```text
current >= absolute minimum
current >= reference - maximum allowed drop
```

Floating point comparisons use a fixed `1e-12` tolerance only for arithmetic
rounding. Reports use stable JSON field names and a sorted violation list.

## 5. CLI and repository layout

```text
evals/rag/                         metric library and strict loaders
evals/cmd/rageval/                 offline CLI
evals/testdata/golden/v1/          immutable Golden Set
evals/testdata/predictions/        checked-in reference predictions
evals/testdata/baselines/          digest-bound release baseline
```

`make eval` runs the CLI against the checked-in v1 inputs and then runs all eval
unit/contract tests. Callers can supply alternate paths to `rageval`; stdout is
the JSON report and a violated gate returns a non-zero exit status. No network,
database, model credential, or wall clock is involved.

## 6. Acceptance checklist

- [x] Golden Set schema and human-review slice are versioned and validated.
- [x] Recall@K, MRR@K, citation correctness, and faithfulness are deterministic.
- [x] Baseline is cryptographically bound to Golden Set bytes.
- [x] Absolute floors and relative regression budgets fail closed.
- [x] One offline command produces a machine-readable report.
