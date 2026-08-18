# Agent Guidance

## Project purpose

This repository is the in-memory matching core for exactly one market. Preserve this boundary: it does not own accounts, balances, risk, persistence, networking, symbol routing, or market-data fan-out.

Read [SPEC.md](SPEC.md) before changing matching semantics.

## Concurrency contract

- Submit(Command) is safe for many producer goroutines.
- Exactly one matching worker calls Process(maxBatch).
- Exactly one dispatcher calls PollResults(dst).
- Do not add mutexes to tree, price-level, order, free-list, or matching code. Those structures are confined to the matching worker.
- Do not expose pointers to internal order or priceLevel nodes.
- A successful Submit only enqueues work. Matching happens during Process.
- Preserve output backpressure: reserve result capacity before removing input commands, and never drop accepted results.

## Domain invariants

- One OrderBook serves one immutable market configuration.
- Price is positive integer ticks; Quantity is positive integer lots. Do not introduce float64 or decimal values in the matching path.
- Price-time priority uses worker-assigned Sequence, not timestamps.
- AcceptedAt is audit metadata from the injected clock.
- Matching is continuous and executes at the resting maker price.
- Same-price orders are FIFO: append at tail; fill and remove from head.
- Partial maker fills retain priority. Incoming unfilled remainder rests at its own price-level tail.
- Cancels are idempotent and use engine-assigned OrderID.
- Buy and sell must share the same side/tree implementation. Only best-price direction differs.
- map[Price]*priceLevel and map[OrderID]*order provide expected O(1) lookup.
- The intrusive red-black tree provides ordered levels. Keep sideBook.best accurate after every insert/remove.
- An empty price level must be removed from both its price map and tree.
- insertLevel rejects nil/duplicate levels; removeLevel rejects nil/stale levels without mutation.

## Memory and performance

- Reuse Order and PriceLevel with the per-book free lists.
- Do not use shared sync.Pool.
- processBuffer is a matching-worker-owned, preallocated scratch slice. Process fills it with MPSC PollVec, applies commands, then reuses it next batch. Do not return it or share it between goroutines.
- A persistent tree node normally lives on the heap. Cold insertion may allocate; warm add/remove using recycled nodes should remain allocation-free.
- Keep Command value-only: no caller-owned pointer, slice, map, or string payloads.

## Tests

Tree changes require updating or preserving [tree_test.go](tree_test.go):

- Verify every red-black invariant after each mutation: black root, black nil leaves, no red parent/child, equal black heights.
- Verify BST order, parent links, map/tree count, map pointer identity, and cached best price.
- Retain deterministic rotation, deletion, boundary, monotonic, 100k differential, and fuzz coverage.
- Run:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem
go test -run '^$' -fuzz=FuzzTreeAgainstMap -fuzztime=10s
```

Use go test -gcflags='-m' -run '^$' when changing allocation-sensitive tree code.

## Repository hygiene

- Use gofmt for every changed Go file.
- Do not modify .idea/; it is user-owned local editor state.
- Keep public type declarations separated by responsibility:
  - value.go: numeric domain values
  - enum.go: enums
  - command.go: input/replay values
  - result.go: output values
  - config.go: configuration and clock
- The MPSC queue dependency is github.com/laplace789/mpsc-ringbuffer.
