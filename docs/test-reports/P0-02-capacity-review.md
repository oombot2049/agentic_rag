# P0-02 validation report: capacity and workload baseline

- Result: Passed
- Date: 2026-08-23
- Validation type: boundary-value and calculation review

## Reconciled calculations

| Calculation | Formula | Result |
|---|---|---:|
| Average Turns/day | `5 × 86,400` | 432,000 |
| Peak Runs in flight | `50 × 45` | 2,250 |
| Provisioned Run slots | `ceil(2,250 × 1.33)` | 2,993, rounded to 3,000 |
| Burst incremental backlog | `(100 - 50) × 60` | 3,000 |
| Peak LLM calls/s | `50 × 2.2` | 110 |
| Peak retrieval calls/s | `50 × 1.6` | 80 |
| Peak event appends/s | `50 × 25` | 1,250 |
| Average model tokens/day | `432,000 × 7,200` | 3,110,400,000 |
| Core row growth/day | `432,000 × 24 KiB` | 9.9 GiB |
| Event growth/day | `432,000 × 20 KiB` | 8.2 GiB |
| Artifact metadata/day | `432,000 × 4 KiB` | 1.6 GiB |
| Artifact payload/day | `432,000 × 160 KiB` | 65.9 GiB |

## Boundary review

- Average, sustained peak, burst and hard cap use different values and meanings.
- Concurrency uses p95 Run duration, not mean duration.
- A 60-second burst fits within the 6,000 pending-Run hard cap.
- Provider slowdown reaches admission pressure before the 4,000 Run safety cap.
- Per-tenant and workload-class limits prevent a hotspot or ingestion batch from
  consuming all interactive capacity.
- Critical degradation rejects early while retaining read, cancel, resume and
  recovery paths.
- No degradation mode weakens tenant isolation, ACL, idempotency or durability.
- Invalid or internally inconsistent limits are required to fail at startup.

## Conclusion

P0-02 covers peak, average, burst and degradation targets with configurable
limits and auditable formulas. P0-03 may now derive latency and availability SLOs
from a named workload rather than from unspecified averages.
