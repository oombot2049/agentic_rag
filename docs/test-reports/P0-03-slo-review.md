# P0-03 validation report: SLO and error budget

- Result: Passed
- Date: 2026-08-23
- Validation type: metric-definition and boundary review

## Reconciled values

| Check | Formula | Result |
|---|---|---:|
| 99.90% time budget over 30 days | `43,200 min × 0.001` | 43.2 min |
| 99.95% time budget over 30 days | `43,200 min × 0.0005` | 21.6 min |
| 99.50% time budget over 30 days | `43,200 min × 0.005` | 216 min |
| 99.99% time budget over 30 days | `43,200 min × 0.0001` | 4.32 min |
| Average commands over 30 days | `5/s × 86,400 × 30` | 12,960,000 |
| 99.90% bad-command budget | `12,960,000 × 0.001` | 12,960 |
| Run timeout allocation | `10 + 3 + 8 + 75 + 12 + 2 + 10` | 120 s |

## Definition review

- Every availability objective defines an eligible and good event.
- Client-caused invalid/auth/quota/cancel outcomes are separated from platform
  overload and dependency failures.
- First token cannot be satisfied by a heartbeat or progress event.
- Retrieval latency includes ACL and rerank, and explicitly excludes LLM query
  rewrite while retaining it in the end-to-end SLO.
- Successful latency percentiles cannot hide timed-out events because timeouts
  also consume availability budget.
- Global and qualifying per-tenant views are both required.
- IDs with unbounded cardinality are prohibited as metric labels.
- Fast, sustained and slow burn paths define concrete operational actions.

## Conclusion

P0-03 meets the plan requirement for availability, first-token, complete-response
and retrieval latency objectives at P50/P95/P99, with reconciled timeout and
error budgets.
