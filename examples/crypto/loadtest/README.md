# Crypto K6 load test

This is a loopback-only HTTP harness around the in-memory order-book core. It is a benchmark tool, not a production exchange API: it has no authentication, accounts, balances, persistence, or market routing.

Start it in one terminal:

```sh
go run ./examples/crypto/loadtest
```

Then run a profile in another:

```sh
k6 run examples/crypto/loadtest/k6/crypto.js
PROFILE=load k6 run examples/crypto/loadtest/k6/crypto.js
PROFILE=stress k6 run examples/crypto/loadtest/k6/crypto.js
```

Profiles are `smoke` (4 VU/10s), `load` (100 VU/60s), and `stress` (500 VU/5m). `BASE_URL`, `SEED`, and `CANCEL_RATIO` (default 5) are configurable environment variables. The server capacities can be adjusted with `-input-capacity`, `-output-capacity`, `-max-active-orders`, `-max-price-levels`, and `-batch-size`.

`POST /orders` and `POST /cancels/random` return `202` only when the command enters the input queue. The dispatcher independently consumes results and builds public aggregate metrics:

- `GET /stats` returns JSON. Add `?wait_ms=10000` after a K6 run to wait for results of all accepted commands so far.
- `GET /metrics` returns Prometheus text metrics.

The K6 script uses deterministic integer price ticks and lot quantities. It drives mostly crossing buys/sells, a small resting flow to create cancellable orders, and 5% random valid cancels. Its teardown verifies that all accepted commands produced a result; stress is allowed to return HTTP 429 as an explicit saturation signal, but HTTP 5xx is always a failure.

## Same-price functional stress test

This separate test verifies a stronger correctness invariant under concurrency: every iteration submits a same-price (`60,000.00 USDT`), same-quantity buy/sell pair. It has no cancels. A `429` is retried with exponential backoff for up to five seconds, so temporary saturation does not unbalance a pair. After all accepted commands drain, the external projection must contain zero active orders.

```sh
k6 run examples/crypto/loadtest/k6/same-price.js
PROFILE=same-price-2000 k6 run examples/crypto/loadtest/k6/same-price.js
```

`same-price-2000` is 2,000 VU for 60 seconds. Set `RETRY_DEADLINE_MS` to change the default 5,000ms retry deadline. A retry deadline expiration, engine rejection, missing result, or residual active order fails the K6 run.
