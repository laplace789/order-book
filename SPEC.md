# Order Book Specification

## 1. Purpose and scope

This project implements the in-memory, continuous-limit-order-book core for one market (one trading pair). It is a low-level component: symbol routing, authentication, balances, risk checks, external market-data fan-out, WAL durability, and account-level self-trade prevention are outside its scope.

The core accepts limit orders and cancellations, applies price-time priority, and emits an ordered result for every accepted input command.

### 1.1 First-version order types

- Limit order with time-in-force GTC.
- Cancel by the engine-assigned `OrderID`.

The first version does not support market, IOC, FOK, post-only, stop, amend/replace, or self-trade-prevention orders.

## 2. Concurrency model

### 2.1 Single writer

Exactly one caller (the matching worker) may mutate or inspect the live book state. The `OrderBook` does not start a goroutine; its owner repeatedly calls `Process(maxBatch)` from one dedicated goroutine.

All tree nodes, price-level lists, maps, free lists, and sequence allocation are confined to that goroutine. They require no mutexes or atomic operations.

### 2.2 Input queue

Commands arrive through [`github.com/laplace789/mpsc-ringbuffer`](https://github.com/laplace789/mpsc-ringbuffer):

- Any number of producers may call `Submit` concurrently.
- The matching worker is the queue's only consumer and uses `PollVec`.
- A full or closed queue makes `Submit` fail immediately with `ErrQueueFull` or `ErrClosed`; it never blocks, spins, retries, or discards an accepted command.
- Producers transfer ownership at successful enqueue. A command is an immutable value-only payload: it must not contain caller-owned pointers, slices, maps, or strings.

The selected MPSC queue is mutex-free and non-blocking at the API level, but does **not** claim formal lock-free progress: a producer paused after reserving a FIFO slot can temporarily prevent the consumer from passing that slot. This project therefore promises a mutex-free single-writer design, not a formal whole-system lock-free progress guarantee.

### 2.3 Output queue

The matching worker is the sole producer of results and a single external event dispatcher is the sole consumer. The dispatcher owns any later fan-out to WAL, market-data, WebSocket, or other downstream systems.

The output queue is bounded. If it has no available slot, `Process` must stop before dequeuing and applying another input command. Results are never dropped.

Each input command produces exactly one immutable `CommandResult` queue item. Its `Trades` slice contains zero or more fills produced by that command. This allows output capacity to be reserved per command even when a taker sweeps many resting orders. Result values must not refer to reusable `Order` or `PriceLevel` nodes.

## 3. Market configuration and numeric representation

One `OrderBook` instance represents one market and has immutable configuration. All capacities are constructor parameters:

- input MPSC capacity;
- output result capacity;
- maximum active orders;
- maximum active price levels;
- default or permitted maximum processing batch size.

Capacities are validated at construction. Maximum active orders and maximum price levels are hard limits, not merely map preallocation hints.

### 3.1 Integer units

The matching hot path uses no `float64` or decimal type.

- `Price` is a positive `int64` number of per-market price ticks.
- `Quantity` is a positive `uint64` number of per-market base-asset lots.

The gateway converts decimal input to ticks/lots before submission. Tick size and lot size are fixed for the life of a book; changing precision requires a new market/book version. The core rejects non-positive price or quantity and rejects aggregate-quantity overflow without wraparound.

## 4. Commands, identity, and time

### 4.1 Command payload

`Command` is a value type with an operation kind:

- `SubmitLimit`: `RequestID`, `Side`, `Price`, and `Quantity`.
- `Cancel`: `RequestID` and engine `OrderID`.

`RequestID` is a producer-provided `uint64` used only to correlate the asynchronous result. It must be unique for the lifetime of the running book; a duplicate is rejected as `ErrDuplicateRequestID`.

On acceptance, the matching worker assigns an increasing, never-reused `OrderID uint64`. Cancellation uses this engine ID. The order index is `map[OrderID]*Order`, making active-order lookup O(1).

### 4.2 Time priority

The dequeued FIFO command order is the sole authoritative time ordering. For every accepted new order, the matching worker assigns:

- an increasing `Sequence`, used for price-time priority; and
- `AcceptedAt`, obtained from an injected `Clock` and encoded as UTC Unix nanoseconds, for audit and market-data purposes.

Timestamp is never used to break priority ties; several commands may share a nanosecond. A resting order retains its original sequence and timestamp after a partial fill.

## 5. Matching semantics

The book continuously matches price-compatible limit orders.

- A buy order matches while `buy.Price >= bestAsk.Price`.
- A sell order matches while `sell.Price <= bestBid.Price`.
- Each fill executes at the resting maker order's price.
- At each price, older resting orders fill before newer ones.
- An incoming order's remaining quantity rests only after all currently compatible opposite prices have been exhausted; it appends to the tail of its own price level.

Cancellation is idempotent:

- An active order is removed and yields `Canceled` with its remaining quantity.
- A missing or terminal order yields `CancelNoop` with a reason (`Unknown`, `Filled`, or `Canceled`) and does not change book state.

Changing price or increasing quantity is intentionally unavailable in v1. Future amend semantics, if added, must be cancel-plus-new and lose time priority unless explicitly re-specified.

## 6. Data structures and complexity

Both sides use one shared internal side implementation. Only the selection of best price differs: bid selects maximum price; ask selects minimum price.

| Structure | Role | Complexity |
| --- | --- | --- |
| `map[Price]*PriceLevel` | Locate an existing price level | O(1) average |
| Intrusive red-black tree of `PriceLevel` | Best price and sorted traversal | O(1) for cached best; O(log P) insert/delete |
| Intrusive doubly linked list in each `PriceLevel` | FIFO orders at one price | O(1) append/remove/head |
| `map[OrderID]*Order` at book scope | Find an active order for cancel | O(1) average |

`PriceLevel` directly owns red-black-tree `parent`, `left`, `right`, and color links. The project will not use `emirpasic/gods`: a separate general-purpose tree node would add pointer indirection and allocation for every price level.

Empty price levels are removed from both the price map and red-black tree. The tree's min/max price levels are cached so best bid/ask lookup is O(1). Price-level insertion/removal remains O(log P), where `P` is active levels.

Active `Order` and `PriceLevel` nodes are recycled through per-book free lists. No shared `sync.Pool` is used. External APIs never expose node pointers, so a recycled node cannot be observed by another goroutine.

## 7. Results and errors

`CommandResult` is emitted in processed-command order and includes the input `RequestID`. A successful new order additionally includes assigned `OrderID`, `Sequence`, and `AcceptedAt`; a matching result carries an ordered value slice of `Trade` records containing maker/taker IDs, maker price, and quantity.

Expected result statuses include `Accepted`, `Rejected`, `Canceled`, and `CancelNoop`. Relevant rejection reasons include invalid price, invalid quantity, quantity overflow, duplicate request ID, input closure, and active order or price-level capacity exhaustion.

No external goroutine may query mutable book maps, trees, or lists. The v1 O(1) price lookup requirement applies to the matching goroutine internally. External readers consume results and build their own view. A snapshot API may be introduced later with an explicit immutable-publication design.

## 8. Lifecycle and recovery

`CloseInput` closes the MPSC input queue. It rejects later submissions but does not cancel resting orders. The matching worker continues processing until input is drained, then results are consumed before its owner releases the book.

The book does not perform I/O or persistence. Its owner must persist commands and/or accepted execution events outside the matching hot path. The core must support a restricted recovery mode that replays already accepted metadata (`OrderID`, `Sequence`, and `AcceptedAt`) instead of creating new IDs or timestamps. This makes reconstruction deterministic.

## 9. Verification and acceptance criteria

The implementation is accepted only after it includes:

- unit tests for price priority, same-price FIFO, partial fills, maker pricing, cancel behavior, empty-level removal, hard capacities, and graceful shutdown;
- randomized differential tests against a simpler reference model;
- red-black-tree invariant tests after every insertion and deletion (black root, no red parent/red child, equal black height on all leaf paths);
- list/map/tree consistency checks;
- `go test -race ./...` passing; and
- benchmarks for one/many-producer input, matching, cancellation, and multi-level sweeps, inspected with `-benchmem` for steady-state allocations.

The module path is `github.com/laplace789/order-book` and the minimum supported Go version is Go 1.25.

