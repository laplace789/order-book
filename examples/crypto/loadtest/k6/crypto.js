import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate } from 'k6/metrics';

const profile = __ENV.PROFILE || 'smoke';
const baseURL = __ENV.BASE_URL || 'http://127.0.0.1:8080';
const seed = Number(__ENV.SEED || '20260818');
const cancelRatio = Number(__ENV.CANCEL_RATIO || '5');

const profiles = {
  smoke: { vus: 4, duration: '10s' },
  load: { vus: 100, duration: '60s' },
  stress: { vus: 500, duration: '5m' },
};
if (!profiles[profile]) throw new Error(`unknown PROFILE=${profile}`);
http.setResponseCallback(http.expectedStatuses(200, 202, 409, 429));

export const options = {
  scenarios: { matching: { executor: 'constant-vus', ...profiles[profile] } },
  thresholds: {
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
    ...(profile === 'stress' ? {} : { orders_queue_full: ['count==0'] }),
  },
};

const submitted = new Counter('orders_accepted');
const queueFull = new Counter('orders_queue_full');
const cancelUnavailable = new Counter('cancel_unavailable');
const accepted = new Rate('ingress_accepted');

// Deterministic xorshift32: every VU/iteration produces reproducible input.
function randomFor(vu, iteration) {
  let value = (seed ^ (vu * 0x9e3779b9) ^ iteration) >>> 0;
  value ^= value << 13; value >>>= 0;
  value ^= value >>> 17; value >>>= 0;
  value ^= value << 5; return value >>> 0;
}

function requestID() { return __VU * 1000000000000 + __ITER + 1; }

export default function () {
  const value = randomFor(__VU, __ITER);
  const id = requestID();
  let response;
  if ((value % 100) < cancelRatio) {
    response = http.post(`${baseURL}/cancels/random`, JSON.stringify({ request_id: id }), { headers: { 'Content-Type': 'application/json' } });
    const ok = check(response, { 'cancel accepted or no active order': r => r.status === 202 || r.status === 409 });
    if (response.status === 409) cancelUnavailable.add(1);
    accepted.add(ok);
    return;
  }

  const side = (value & 1) === 0 ? 'buy' : 'sell';
  const shape = Math.floor(value / 100) % 20;
  let price;
  if (shape === 0) price = side === 'buy' ? 5997000 : 6003000; // resting flow, feeds valid cancels
  else price = side === 'buy' ? 6000000 + (value % 2001) : 5998000 + (value % 2001);
  const quantity = 100 + (Math.floor(value / 4096) % 9901); // 0.0100-1.0000 BTC in integer lots
  response = http.post(`${baseURL}/orders`, JSON.stringify({ request_id: id, side, price, quantity }), { headers: { 'Content-Type': 'application/json' } });
  const ok = check(response, { 'order accepted or backpressured': r => r.status === 202 || r.status === 429 });
  if (response.status === 202) submitted.add(1);
  if (response.status === 429) queueFull.add(1);
  accepted.add(ok);
}

export function teardown() {
  const response = http.get(`${baseURL}/stats?wait_ms=10000`);
  check(response, {
    'stats endpoint succeeds': r => r.status === 200,
    'accepted inputs fully drained': r => {
      const stats = r.json();
      return stats.in_flight === 0 && stats.submitted === stats.processed_results;
    },
    'no server-side bad requests': r => r.json().bad_request === 0,
  });
}

export function handleSummary(data) {
  return { stdout: JSON.stringify({ profile, metrics: data.metrics }, null, 2) };
}
