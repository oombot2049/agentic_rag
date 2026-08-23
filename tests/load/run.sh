#!/usr/bin/env bash
set -euo pipefail

: "${AGENTICRAG_LOAD_ACK:?set AGENTICRAG_LOAD_ACK}"
: "${BASE_URL:?set BASE_URL}"
: "${API_TOKEN:?set API_TOKEN}"
: "${PROFILE:?set PROFILE}"
: "${SESSION_POOL_FILE:?set SESSION_POOL_FILE}"
: "${RUN_POOL_FILE:?set RUN_POOL_FILE}"
: "${RESULT_DIR:?set RESULT_DIR}"
: "${STAGE_NAME:?set STAGE_NAME}"

[[ "$AGENTICRAG_LOAD_ACK" == "production-load-authorized" ]] || { echo "load authorization acknowledgement is invalid" >&2; exit 2; }
[[ ! -e "$RESULT_DIR" ]] || { echo "RESULT_DIR must not already exist" >&2; exit 2; }
command -v k6 >/dev/null
command -v curl >/dev/null
command -v go >/dev/null

evidence_class="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -field evidence-class)"
if [[ "$evidence_class" == "RELEASE" && "$BASE_URL" =~ ^https?://(localhost|127\.0\.0\.1|\[::1\])([:/]|$) ]]; then
  echo "release evidence cannot target loopback" >&2
  exit 2
fi

export TURN_RATE="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field turn-rps)"
duration_seconds="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field duration-seconds)"
export DURATION="${duration_seconds}s"
export EXPECTED_REQUESTS="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field expected-requests)"
SSE_CONNECTIONS="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field sse-connections)"
SSE_RECONNECT_RATE="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field sse-reconnect-rps)"
has_reconnect="$(go run ./tests/load/cmd/profilemeta -profile "$PROFILE" -stage "$STAGE_NAME" -field has-reconnect)"
export TURN_RATE DURATION EXPECTED_REQUESTS SSE_CONNECTIONS SSE_RECONNECT_RATE STAGE_NAME

mkdir -m 700 "$RESULT_DIR"
umask 077
export AGENTICRAG_LOAD_TOKEN="$API_TOKEN"
children=()
cleanup() {
  for pid in "${children[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM
curl --fail --silent --show-error "$BASE_URL/metrics" -o "$RESULT_DIR/metrics-before.prom"
curl --fail --silent --show-error --max-time 35 "$BASE_URL/debug/pprof/profile?seconds=30" -o "$RESULT_DIR/cpu.pprof" &
pprof_pid=$!
children+=("$pprof_pid")

go run ./tests/load/cmd/sse-load -base-url "$BASE_URL" -run-pool "$RUN_POOL_FILE" -mode connections -connections "$SSE_CONNECTIONS" -duration "$DURATION" -output "$RESULT_DIR/sse-connections.json" &
sse_pid=$!
children+=("$sse_pid")
if [[ "$has_reconnect" == "true" ]]; then
  go run ./tests/load/cmd/sse-load -base-url "$BASE_URL" -run-pool "$RUN_POOL_FILE" -mode reconnect -reconnect-rate "$SSE_RECONNECT_RATE" -duration "$DURATION" -output "$RESULT_DIR/sse-reconnect.json" &
  reconnect_pid=$!
  children+=("$reconnect_pid")
fi
k6 run --summary-export "$RESULT_DIR/k6-summary.json" ./tests/load/k6/create-turn.js
wait "$sse_pid"
if [[ "$has_reconnect" == "true" ]]; then
  wait "$reconnect_pid"
fi
wait "$pprof_pid"
curl --fail --silent --show-error "$BASE_URL/debug/pprof/heap" -o "$RESULT_DIR/heap.pprof"
curl --fail --silent --show-error "$BASE_URL/metrics" -o "$RESULT_DIR/metrics-after.prom"
trap - EXIT INT TERM

echo "raw evidence captured in $RESULT_DIR; normalize measurements and run loadreport before claiming capacity"
