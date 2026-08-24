# AgenticRAG dashboards

Mount the two JSON files at `/var/lib/grafana/dashboards/agenticrag` and the
provisioning file under Grafana's provisioning directory. Configure a Prometheus
datasource and a PostgreSQL-compatible governed analytics datasource. The latter
must expose only these contracts:

- `dashboard_tenant_scope(tenant_key, display_name)` with viewer-aware RLS;
- `analytics_run_funnel_hourly(bucket_start, tenant_key, run_id, stage)` with one
  idempotent row per Run/stage;
- `analytics_run_business_hourly(bucket_start, tenant_key, committed_runs,
  terminal_runs, terminal_successes, cost_nanos)`;
- `analytics_run_reconciliation_hourly(bucket_start, tenant_key,
  projection_lag_seconds, source_terminal_runs, projected_terminal_runs,
  source_committed_runs, projected_committed_runs)`.

The analytics role is read-only and cannot bypass RLS. Dashboard provisioning
disables UI updates; changes are reviewed in Git. Prometheus contains no tenant
selector. Billing and reconciliation always use durable analytics facts, not the
floating-point operational cost counter.

These files are deployment artifacts, not proof that the application currently
exports every referenced metric. The composed `serve` role exposes `/healthz`
but not `/metrics`; a deployment must wire the repository's metrics recorders,
Prometheus endpoint, PostgreSQL exporter, and governed analytics views before
the dashboards or alert rules are operational.
