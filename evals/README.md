# AgenticRAG offline evaluation

Run the checked-in release gate without network or credentials:

```sh
make eval
```

To score a new retrieval strategy, emit the prediction contract used by
`testdata/predictions/reference-v1.json` and run:

```sh
go run ./evals/cmd/rageval \
  -golden ./evals/testdata/golden/v1/dataset.json \
  -predictions /path/to/new-strategy.json \
  -baseline ./evals/testdata/baselines/v1.json
```

The command writes one JSON report to stdout and exits non-zero if an absolute
floor or relative regression budget is violated. Input errors never print
partial metrics.

## Dataset governance

- Never rewrite a released Golden Set in place. Create `golden/v2`, assign a new
  dataset/revision ID, obtain human review, and generate a new digest-bound
  baseline in the same change.
- Keep case and evidence IDs stable. Add provenance that lets a reviewer locate
  the source snapshot; do not place secrets or customer data in fixtures.
- At least 20% of cases must be human reviewed with a non-empty review note.
  Favor difficult ambiguity, authorization, negative, and temporal cases rather
  than only easy fact lookups.
- LLM Judge output may be collected as an additional experiment. It cannot
  replace the deterministic metrics or the human-reviewed slice.

## Interpretation

Recall@K and MRR@K evaluate ranked Chunk IDs. Citation correctness is set F1,
so both missing and invented Artifact IDs reduce the score. Faithfulness uses
human-authored claim phrases and their allowed supporting Artifact IDs;
annotated contradictions are unsupported assertions. These metrics are macro
averages across cases.

The checked-in reference predictions are a contract fixture, not a claim about
production model quality. A production evaluation export should be generated
from immutable retrieval audit snapshots and evaluated against the same Golden
Set revision.
