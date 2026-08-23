import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Rate, Trend } from 'k6/metrics';

const sessions = new SharedArray('session-pool', () => JSON.parse(open(__ENV.SESSION_POOL_FILE)).sessions);
const distribution = JSON.parse(open('../testdata/response-sizes-v1.json')).buckets;
const turnLatency = new Trend('create_turn_ms', true);
const turnErrors = new Rate('create_turn_errors');
const requestBytes = new Counter('request_bytes');

export const options = {
  discardResponseBodies: false,
  scenarios: {
    create_turn: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.TURN_RATE || 1),
      timeUnit: '1s',
      duration: __ENV.DURATION || '10s',
      preAllocatedVUs: Number(__ENV.PREALLOCATED_VUS || 100),
      maxVUs: Number(__ENV.MAX_VUS || 1000),
    },
  },
  thresholds: {
    create_turn_errors: ['rate<=0.0005'],
    create_turn_ms: ['p(95)<=250', 'p(99)<=500'],
    checks: ['rate>=0.9995'],
  },
};

export function setup() {
  const expected = Number(__ENV.EXPECTED_REQUESTS || 0);
  if (!__ENV.BASE_URL || !__ENV.API_TOKEN || !Number.isInteger(expected) || expected < 1) throw new Error('BASE_URL, API_TOKEN, and EXPECTED_REQUESTS are required');
  if (sessions.length < expected) throw new Error(`session pool ${sessions.length} is smaller than expected requests ${expected}`);
  const totalWeight = distribution.reduce((sum, item) => sum + item.weight, 0);
  if (totalWeight !== 100) throw new Error(`response-size weights must total 100, got ${totalWeight}`);
}

function bucket(iteration) {
  const slot = iteration % 100;
  let boundary = 0;
  for (const item of distribution) { boundary += item.weight; if (slot < boundary) return item.bytes; }
  throw new Error('invalid response-size distribution');
}

export default function () {
  const index = exec.scenario.iterationInTest;
  const session = sessions[index];
  if (!session) throw new Error(`session pool exhausted at iteration ${index}`);
  const targetBytes = bucket(index);
  const envelope = { expected_context_version: session.expected_context_version, message: { content: '' }, execution: { max_steps: 4 } };
  const emptyBytes = JSON.stringify(envelope).length;
  if (targetBytes <= emptyBytes) throw new Error(`bucket ${targetBytes} cannot fit request envelope ${emptyBytes}`);
  envelope.message.content = 'x'.repeat(targetBytes - emptyBytes);
  const body = JSON.stringify(envelope);
  if (body.length !== targetBytes) throw new Error(`serialized request ${body.length} differs from bucket ${targetBytes}`);
  const response = http.post(`${__ENV.BASE_URL.replace(/\/$/, '')}/v1/sessions/${session.session_id}/turns`, body, {
    headers: { Authorization: `Bearer ${__ENV.API_TOKEN}`, 'Content-Type': 'application/json', 'Idempotency-Key': `${__ENV.LOAD_RUN_ID || 'load'}-${String(index).padStart(12, '0')}` },
    timeout: '25s', tags: { stage: __ENV.STAGE_NAME || 'unknown' },
  });
  const ok = check(response, { 'turn accepted': (r) => r.status === 202 });
  turnLatency.add(response.timings.duration); turnErrors.add(!ok); requestBytes.add(body.length);
}
