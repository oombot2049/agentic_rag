# Requirement P7-02: Bounded Prometheus Metrics

- Status: Complete
- Priority: P0
- Owner: Go Backend
- Last reviewed: 2026-08-23
- Depends on: P4-02 Agent Worker, P7-01 OpenTelemetry Tracing
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P7-02

## 1. Objective

Expose production Prometheus metrics for request rate, latency, active Run
concurrency, queue wait, Run duration/Steps, LLM time-to-first-token, Tokens and
cost. Metrics use a private Registry and a closed label vocabulary so a tenant
or request cannot create an unbounded time series.

```mermaid
flowchart LR
  H[HTTP observation] --> M[Metrics registry]
  W[Run start/finish] --> M
  L[LLM observation] --> M
  M --> P[/metrics Prometheus handler]
  M --> A[Rate/latency/cost alerts]
  T[Tenant ID / Run ID / query detail] --> D[Structured logs / warehouse]
```

## 2. Metric contract

| Family | Type | Labels |
|---|---|---|
| `agenticrag_http_requests_total` | Counter | `plan,method,status_class` |
| `agenticrag_http_request_duration_seconds` | Histogram | same |
| `agenticrag_worker_active_runs` | Gauge | `plan` |
| `agenticrag_run_queue_wait_seconds` | Histogram | `plan` |
| `agenticrag_runs_total` | Counter | `plan,outcome` |
| `agenticrag_run_duration_seconds` / `run_steps` | Histogram | `plan,outcome` |
| `agenticrag_llm_requests_total` | Counter | `plan,provider,model,outcome` |
| `agenticrag_llm_ttft_seconds` | Histogram | `plan,provider,model` |
| `agenticrag_llm_input_tokens_total` / `output_tokens_total` | Counter | `plan,provider,model` |
| `agenticrag_llm_cost_nanos_total` | Counter | `plan,provider,model` |
| `agenticrag_sli_events_total` | Counter | closed `sli,classification` vocabularies |

QPS is derived with `rate(agenticrag_http_requests_total[window])`; it is not a
second mutable gauge. Histograms use explicit service-oriented buckets.

## 3. Cardinality budget

Plans are exactly `free`, `standard`, `enterprise`, or `unknown`. HTTP methods,
status classes, Run outcomes and LLM outcomes are closed enums. Provider/model
pairs come only from a constructor-time allowlist bounded to 32 providers and
128 total models; every other pair collapses to `other/other`.

There is intentionally no API field and no label named tenant/session/turn/Run,
Tool Call, request, query, Chunk or error message. Tenant plan is the only
tenant-derived dimension. Per-tenant diagnosis and billing reconciliation use
structured logs, immutable audit records and the analytics warehouse. The
floating-point Prometheus cost counter is for alerting/trend analysis, never
the billing source of truth.

## 4. Concurrency and lifecycle

`StartRun` increments one plan gauge and returns a single-use handle. `Finish`
validates a closed outcome plus nonnegative duration/Step count, then decrements
exactly once and records terminal histograms/counter. Duplicate Finish calls
return a conflict without double-decrementing. Prometheus collectors provide
concurrent updates; no package-global default registry is mutated.

HTTP middleware preserves optional ResponseWriter interfaces through
`httpsnoop`. Invalid observations return an error instead of panicking or
creating a surprise series. TTFT equal to zero means the non-streaming provider
did not expose it and is omitted rather than substituting total latency.

P7-06 extends the contract with logical SLI events. The owning business boundary
records exactly one `good`, `bad`, or `excluded` event after idempotency and
eligibility classification; dependency attempts and retries do not increment
this logical counter. This lets error-budget alerts distinguish a bad global
overload `429` from an excluded tenant-quota `429` without a high-cardinality
reason label.

## 5. Test strategy

Tests gather and parse real metric families after HTTP, concurrent Run and LLM
observations. A cardinality attack submits thousands of arbitrary plan,
provider, model and method strings and asserts series remain under the closed
budget with no attacker value or forbidden label name in exposition. Tests also
cover duplicate Finish, invalid numeric values, TTFT omission and middleware
interface/status preservation.

## 6. Acceptance checklist

- [x] QPS, concurrency, queue wait, TTFT, Steps, Tokens and cost are exported.
- [x] Metrics aggregate by a four-value tenant plan label.
- [x] Provider/model cardinality is constructor bounded with `other` collapse.
- [x] Tenant and business resource IDs cannot enter labels.
- [x] Run gauge increments/decrements exactly once under concurrency.
