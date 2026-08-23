# Production load evidence

This suite measures Create Turn throughput, SSE occupancy/reconnect latency,
TTFT, queue growth, database pressure, and service resources against the P0
capacity contract. `profiles/release-v1.json` is the authoritative release
shape; `smoke-v1.json` and `testdata/contract-pass.json` only test tooling.

## Evidence boundary

`make load-contract` proves that profiles, strict measurement parsing, limits,
capacity curves, and degradation-point selection are executable. It does not
prove the service has P0 capacity. Only a passing release-profile measurement
with `representative=true`, an immutable build, production-equivalent topology
and retained raw artifacts is capacity evidence.

## Pools and workload

Provision one idle Session per expected Create Turn before measurement:

```json
{"sessions":[{"session_id":"ses_...","expected_context_version":0}]}
```

Provision distinct non-terminal Runs for SSE:

```json
{"run_ids":["run_...","run_..."]}
```

The k6 generator fails before load if the Session pool is too small and creates
exactly one mutation per Session. Request bodies follow the checked-in
256 B/4 KiB/16 KiB/64 KiB distribution. Assistant outputs use the separately
versioned 1/4/16/64 KiB distribution; a pinned model/tool stub must reproduce
those buckets for repeatable platform tests, while a real-provider run records
the observed distribution. Stable idempotency keys identify the
logical attempt without turning retries into duplicate work.

## Authorized representative run

Run from a dedicated load-generator host. The API token needs only the tested
tenant. Do not put it in files or shell history. Export the variables required
by `run.sh`, including `AGENTICRAG_LOAD_ACK=production-load-authorized`, plus
`LOAD_RUN_ID` and `STAGE_NAME`. Rate, duration, expected requests, SSE occupancy,
and reconnect rate are derived from the selected profile stage; they cannot be
overridden independently. Use a fresh absolute `RESULT_DIR` per stage. The
Turn, connection, reconnect, and pprof workloads run concurrently.

Capture the deployment manifest, autoscaling bounds, database shape, model/tool
stubs or pinned providers, build revision and UTC interval beside the raw
artifacts. Normalize k6/SSE/Prometheus results into the measurement schema, then:

```sh
go run ./tests/load/cmd/loadreport \
  -profile ./tests/load/profiles/release-v1.json \
  -measurement /evidence/measurement.json \
  -output /evidence/capacity-report.json
```

The first failing stage is the degradation point. A sustained positive queue
slope always fails even if ending depth is below the hard limit.
