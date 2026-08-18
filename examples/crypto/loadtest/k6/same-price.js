import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate } from 'k6/metrics';

const profile = __ENV.PROFILE || 'smoke';
const baseURL = __ENV.BASE_URL || 'http://127.0.0.1:8080';
const seed = Number(__ENV.SEED || '20260818');
const retryDeadlineMs = Number(__ENV.RETRY_DEADLINE_MS || '5000');
const price = 6000000; // 60,000.00 USDT, expressed in integer ticks.

const profiles = {
  smoke: { vus: 4, duration: '10s' },
  'same-price-2000': { vus: 2000, duration: '60s' },
};
if (!profiles[profile]) throw new Error(`unknown PROFILE=${profile}`);
http.setResponseCallback(http.expectedStatuses(200, 202, 429));

export const options = {
  scenarios: { same_price: { executor: 'constant-vus', ...profiles[profile] } },
  thresholds: {
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
    pair_retry_timeout: ['count==0'],
  },
};

const acceptedOrders = new Counter('same_price_orders_accepted');
const pairsCompleted = new Counter('same_price_pairs_completed');
const queueFull = new Counter('same_price_queue_full');
const retryTimeout = new Counter('pair_retry_timeout');
const pairAccepted = new Rate('same_price_pair_accepted');

function randomFor(vu, iteration) {
  let value = (seed ^ (vu * 0x9e3779b9) ^ iteration) >>> 0;
  value ^= value << 13; value >>>= 0;
  value ^= value >>> 17; value >>>= 0;
  value ^= value << 5; return value >>> 0;
}

function postUntilAccepted(body) {
  const deadline = Date.now() + retryDeadlineMs;
  let backoffMs = 1;
  while (Date.now() < deadline) {
    const response = http.post(`${baseURL}/orders`, JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } });
    if (response.status === 202) {
      acceptedOrders.add(1);
      return true;
    }
    if (response.status !== 429) {
      check(response, { 'order is accepted or explicitly backpressured': r => r.status === 202 || r.status === 429 });
      return false;
    }
    queueFull.add(1);
    sleep(backoffMs / 1000);
    backoffMs = Math.min(backoffMs * 2, 50);
  }
  retryTimeout.add(1);
  return false;
}

export default function () {
  const quantity = 100 + (randomFor(__VU, __ITER) % 9901); // 0.0100-1.0000 BTC, integer lots.
  const pairID = __VU * 10000000000 + (__ITER * 2) + 1;

  // The pair is sequential for admission correctness; VUs remain concurrent.
  const buyAccepted = postUntilAccepted({ request_id: pairID, side: 'buy', price, quantity });
  if (!buyAccepted) { pairAccepted.add(false); return; }
  const sellAccepted = postUntilAccepted({ request_id: pairID + 1, side: 'sell', price, quantity });
  const complete = sellAccepted;
  pairAccepted.add(complete);
  if (complete) pairsCompleted.add(1);
}

export function teardown() {
  const response = http.get(`${baseURL}/stats?wait_ms=30000`);
  check(response, {
    'stats endpoint succeeds': r => r.status === 200,
    'all accepted commands produced results': r => {
      const stats = r.json();
      return stats.in_flight === 0 && stats.submitted === stats.processed_results;
    },
    'every successful pair fully matched': r => r.json().active_projection === 0,
    'no engine rejections': r => r.json().rejected === 0,
    'no malformed requests': r => r.json().bad_request === 0,
  });
}

export function handleSummary(data) {
  return { stdout: JSON.stringify({ profile, metrics: data.metrics }, null, 2) };
}
