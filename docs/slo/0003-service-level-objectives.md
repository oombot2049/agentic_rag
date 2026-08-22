# Requirement P0-03: Service level objectives and error budgets

- Status: Accepted
- Priority: P0
- Owner: Architecture and SRE
- Last reviewed: 2026-08-23
- Depends on: [P0-02 capacity baseline](../capacity/0002-capacity-and-workload-baseline.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P0-03

## 1. Scope

These objectives apply to the MVP workload defined by P0-02: up to 50 accepted
Turn commands/second sustained for 30 minutes, 3,000 provisioned concurrent Runs
and 20,000 concurrent SSE connections. Results outside the tested capacity
envelope remain measured but do not justify weakening isolation or durability.

An SLO is a rolling production objective, not a per-request guarantee. Client
timeouts and internal deadlines are hard behavior contracts and are defined
separately from percentile objectives.

## 2. Common measurement rules

### 2.1 Windows and aggregation

- Compliance window: rolling 30 days, evaluated every five minutes.
- Primary scope: production by region and service version.
- Report both global traffic-weighted compliance and per-tenant compliance for
  tenants with at least 10,000 eligible events in the window.
- Percentiles come from event histograms, never averages of instance-level
  percentiles.
- Planned maintenance counts unless the public service is deliberately declared
  unavailable before an applicable customer contract says otherwise.

### 2.2 Eligible and excluded events

Eligible events are authenticated production requests for supported operations.
The following do not count as service failures:

- client validation errors, authentication failures and forbidden access;
- a correctly enforced tenant-specific quota or same-Session `ACTIVE_RUN`
  conflict;
- explicit client cancellation before the objective completes;
- client network disconnect after a durable event is committed;
- unsupported inputs documented before request acceptance.

The following always count as service failures:

- platform-wide overload rejection or an internally generated `429` caused by
  insufficient planned capacity;
- `5xx`, panic, deadline exceeded, corrupted response or missing durable state;
- dependency failure visible to the client, including configured LLM, retrieval,
  MCP, queue, database or object storage providers;
- cross-tenant exposure or an incorrect successful response. Security incidents
  also invoke the release-stop policy regardless of numerical budget.

Retries do not erase failures. Request-level availability measures the final
response to one logical idempotent command; dependency SLIs measure every
physical attempt separately.

## 3. Availability objectives

Availability uses good-events / eligible-events:

```text
availability = good_events / eligible_events
error_budget = 1 - objective
```

| SLI | Good event | 30-day objective | 30-day time-equivalent budget |
|---|---|---:|---:|
| Turn command availability | Stable success/idempotent replay/conflict response within 2 s and durable reservation when accepted | 99.90% | 43.2 min |
| Session/read availability | Correct tenant-scoped response within read deadline | 99.95% | 21.6 min |
| SSE connect/resume availability | Subscription accepted and replay begins at the requested valid cursor within 5 s | 99.90% | 43.2 min |
| Accepted Run terminalization | Durable terminal state exists within 120 s | 99.50% | 216 min |
| Retrieval availability | Authorized, schema-valid result or documented empty result within 1.5 s | 99.90% | 43.2 min |
| Final commit durability | A reported completed Run has exactly one selected Message readable after cache/event loss | 99.99% | 4.32 min |

At the 5 Turn/s average baseline, Turn commands total 12,960,000 over 30 days.
The 99.90% command budget is therefore at most 12,960 bad logical commands in
that traffic population. The ratio, not this example count, is authoritative.

Security isolation and committed-result correctness have a zero-tolerance gate:
one confirmed cross-tenant read/write or acknowledged-but-missing final commit
stops release even if an availability percentage would otherwise pass.

## 4. Latency objectives

Latency is measured end to end using server monotonic clocks and propagated
trace timestamps. Percentile objectives apply only when the corresponding event
is successful; timed-out events also count against availability.

| SLI | Start | End | P50 | P95 | P99 | Hard deadline |
|---|---|---|---:|---:|---:|---:|
| Create Turn | first authenticated request byte | durable reservation response flushed | 75 ms | 250 ms | 500 ms | 2 s |
| Time to first assistant token | Turn accepted | first assistant content token durably sequenced | 2.5 s | 8 s | 20 s | 45 s |
| Complete response | Turn accepted | terminal event and final Message durably committed | 12 s | 45 s | 90 s | 120 s |
| Retrieval pipeline | normalized authorized query enters Retriever | ranked authorized chunks returned | 100 ms | 400 ms | 1 s | 1.5 s |
| SSE resume | valid subscription received | first replayed/live event flushed | 150 ms | 1 s | 3 s | 5 s |
| Event delivery freshness | event committed to outbox | event flushed to connected non-slow client | 250 ms | 1 s | 3 s | 5 s |
| Cancel acknowledgement | cancel command received | durable cancel request acknowledged | 75 ms | 250 ms | 500 ms | 2 s |
| Cancel effectiveness | durable cancel requested | Run reaches terminal state or active provider call is fenced | 500 ms | 2 s | 5 s | 10 s |

The retrieval SLI includes hybrid retrieval, ACL filtering and reranking but
excludes an optional LLM query-rewrite Step; that Step remains inside the first-
token and complete-response objectives. Empty authorized results are successful;
unscoped or schema-invalid results are failures.

The first-token SLI counts an assistant content token, not heartbeat, queue,
trace or tool-progress events. This prevents operational events from making user
latency look artificially good.

## 5. End-to-end timeout budget

The 120-second Run deadline begins when the Turn is accepted. The Agent runtime
allocates a shared deadline rather than granting every Step its maximum timeout.

| Budget component | Maximum cumulative allocation | Notes |
|---|---:|---|
| Queue wait and lease acquisition | 10 s | Reject/defer before acceptance when capacity cannot honor this |
| Context build and token budgeting | 3 s | PostgreSQL/object reads included |
| All retrieval and MCP retrieval calls | 8 s | 1.5 s per online attempt; bounded retry budget |
| All LLM calls | 75 s | 45 s per call; shared Run deadline wins |
| All non-LLM tools | 12 s | Longer work must use an asynchronous tool contract |
| Final commit and event publication | 2 s | PostgreSQL transaction plus durable outbox |
| Scheduling/retry/safety reserve | 10 s | Never preallocated to a provider |
| **Total** | **120 s** | Hard Run deadline |

Rules:

1. Each outbound request receives `min(component_remaining, run_remaining)`.
2. Retries require enough remaining budget for backoff plus a useful attempt.
3. No new Step begins after the safety reserve is entered unless it is final
   commit, cancellation or failure recording.
4. Timeout creates an explicit terminal reason and does not roll back already
   committed audit/Attempt data.
5. The client request timeout for POST Turn is 2 seconds; Run progress continues
   independently after an accepted durable reservation.

SSE heartbeat interval is 15 seconds. Proxies must allow at least 45 seconds of
idle time, and clients reconnect with bounded exponential backoff and
`Last-Event-ID`. Heartbeats do not extend the Run deadline.

## 6. Error-budget policy

### 6.1 Burn rate

For an objective `S`, budget burn rate over a window is:

```text
burn_rate = observed_bad_ratio / (1 - S)
```

`1×` consumes exactly one 30-day budget over 30 days. Alerts require both a
short and long window to reduce noise:

| Severity | Long window | Short confirmation | Burn threshold | Action |
|---|---:|---:|---:|---|
| Page: fast burn | 1 h | 5 min | ≥14.4× in both | Incident response; stop risky rollout |
| Page: sustained burn | 6 h | 30 min | ≥6× in both | Incident response and owner escalation |
| Ticket: slow burn | 3 d | 6 h | ≥1× in both | Prioritized reliability work |
| Security/durability gate | any | any | one confirmed event | Stop release; incident process |

### 6.2 Release policy

- More than 50% of a monthly budget consumed before day 15 freezes non-
  reliability production changes for the affected service.
- Exhausted budget freezes feature rollout until the SLO returns inside budget
  or an owner approves a documented exception with mitigation and expiry.
- A new release cannot use retry amplification or relaxed deadlines merely to
  hide an SLO miss.
- Tenant-specific misses are investigated even when global traffic remains good;
  high-volume tenants cannot mask starvation of smaller tenants.

## 7. Metrics contract

Required metric families:

```text
agenticrag_http_requests_total{route,method,status_class,reason}
agenticrag_http_duration_seconds{route,method}
agenticrag_run_terminal_total{status,reason}
agenticrag_run_duration_seconds{status}
agenticrag_first_token_seconds{provider,model_class}
agenticrag_retrieval_requests_total{adapter,status,reason}
agenticrag_retrieval_duration_seconds{adapter}
agenticrag_sse_resume_total{status,reason}
agenticrag_sse_resume_seconds{status}
agenticrag_event_delivery_lag_seconds{consumer_class}
agenticrag_final_commit_total{status,reason}
```

Raw `tenant_id`, Session, Turn, Run or request IDs are trace/log attributes and
must not be unbounded metric labels. Tenant SLO reports use an offline/streaming
aggregation keyed by an approved tenant dimension, not Prometheus cardinality.

Every histogram must expose buckets capable of resolving its P50/P95/P99 target
and hard deadline. Every counter documents eligible, good, excluded and bad
reason-code mappings in the owning dashboard/query.

## 8. Validation plan

1. Unit-test SLI classification for each status and reason code.
2. Generate synthetic histogram distributions that sit just below and above
   every percentile threshold.
3. Verify timeout propagation with fake clocks and providers.
4. Run sustained-peak load at 50 Turn/s and 20,000 SSE connections.
5. Inject LLM, retrieval, Redis/event and PostgreSQL failures and reconcile
   metric counts with durable records.
6. Test multi-window burn alerts with recorded time series.
7. Compare global and per-tenant reports under an intentional tenant hotspot.

## 9. Acceptance checklist

- [x] Availability and latency SLIs have precise eligible/good definitions.
- [x] First-token, complete-response and retrieval P50/P95/P99 are explicit.
- [x] Hard request, Run, retrieval, SSE and cancellation deadlines are explicit.
- [x] The 120-second Run timeout budget reconciles exactly.
- [x] Thirty-day error budgets and multi-window burn alerts are defined.
- [x] Capacity-overload rejections count as failures while tenant policy
  rejections are excluded.
- [x] Tenant starvation and security/durability failures cannot hide in a global
  percentage.
- [x] Metric cardinality and histogram requirements are explicit.

## 10. Change control

Objectives may tighten after production measurement. Relaxing an objective,
changing eligibility or excluding a failure category requires an ADR, before/
after budget analysis and approval from the service owner and SRE. Provider
changes must run the P0-03 validation plan before rollout.
