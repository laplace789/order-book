package orderbook

import (
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func newTestBook(t *testing.T) *OrderBook {
	t.Helper()
	book, err := New(Config{
		InputCapacity:    32,
		OutputCapacity:   32,
		MaxActiveOrders:  32,
		MaxPriceLevels:   32,
		DefaultBatchSize: 16,
		Clock:            fixedClock{now: time.Unix(1_700_000_000, 123).UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return book
}

func submitAndProcess(t *testing.T, book *OrderBook, command Command) CommandResult {
	t.Helper()
	if err := book.Submit(command); err != nil {
		t.Fatalf("Submit(%+v): %v", command, err)
	}
	if n := book.Process(1); n != 1 {
		t.Fatalf("Process = %d, want 1", n)
	}
	var results [1]CommandResult
	if n := book.PollResults(results[:]); n != 1 {
		t.Fatalf("PollResults = %d, want 1", n)
	}
	return results[0]
}

func TestMakerPriceAndPriceTimePriority(t *testing.T) {
	book := newTestBook(t)
	first := submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 1, Side: Sell, Price: 100, Quantity: 4})
	second := submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 2, Side: Sell, Price: 100, Quantity: 6})
	taker := submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 3, Side: Buy, Price: 105, Quantity: 7})

	if taker.Status != Accepted || taker.Remaining != 0 {
		t.Fatalf("taker = %+v", taker)
	}
	if len(taker.Trades) != 2 {
		t.Fatalf("trades = %+v, want two fills", taker.Trades)
	}
	if got := taker.Trades[0]; got.MakerOrderID != first.OrderID || got.Price != 100 || got.Quantity != 4 {
		t.Fatalf("first trade = %+v", got)
	}
	if got := taker.Trades[1]; got.MakerOrderID != second.OrderID || got.Price != 100 || got.Quantity != 3 {
		t.Fatalf("second trade = %+v", got)
	}
	remaining := book.orders[second.OrderID]
	if remaining == nil || remaining.remaining != 3 || remaining.sequence != second.Sequence {
		t.Fatalf("remaining maker = %+v", remaining)
	}
	assertBookInvariants(t, book)
}

func TestCancelAndPriceLevelRemoval(t *testing.T) {
	book := newTestBook(t)
	order := submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 1, Side: Buy, Price: 99, Quantity: 5})
	canceled := submitAndProcess(t, book, Command{Kind: Cancel, RequestID: 2, OrderID: order.OrderID})
	if canceled.Status != Canceled || canceled.Remaining != 5 {
		t.Fatalf("cancel = %+v", canceled)
	}
	if book.bids.best != nil || book.priceLevels != 0 || book.activeOrders != 0 {
		t.Fatalf("book was not emptied")
	}
	noop := submitAndProcess(t, book, Command{Kind: Cancel, RequestID: 3, OrderID: order.OrderID})
	if noop.Status != CancelNoop || noop.Reason != CanceledOrder {
		t.Fatalf("noop = %+v", noop)
	}
	assertBookInvariants(t, book)
}

func TestCapacityAndDuplicateRequest(t *testing.T) {
	book, err := New(Config{
		InputCapacity: 2, OutputCapacity: 2, MaxActiveOrders: 1, MaxPriceLevels: 1,
		DefaultBatchSize: 1, Clock: fixedClock{now: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 1, Side: Buy, Price: 10, Quantity: 1})
	capacity := submitAndProcess(t, book, Command{Kind: SubmitLimit, RequestID: 2, Side: Buy, Price: 10, Quantity: 1})
	if capacity.Status != Rejected || capacity.Reason != OrderCapacityExceeded {
		t.Fatalf("capacity result = %+v", capacity)
	}
	duplicate := submitAndProcess(t, book, Command{Kind: Cancel, RequestID: 2, OrderID: 123})
	if duplicate.Status != Rejected || duplicate.Reason != DuplicateRequestID {
		t.Fatalf("duplicate result = %+v", duplicate)
	}
}

func TestOutputBackpressureAndGracefulDrain(t *testing.T) {
	book, err := New(Config{
		InputCapacity: 8, OutputCapacity: 2, MaxActiveOrders: 8, MaxPriceLevels: 8,
		DefaultBatchSize: 8, Clock: fixedClock{now: time.Unix(0, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for requestID := uint64(1); requestID <= 3; requestID++ {
		if err := book.Submit(Command{Kind: SubmitLimit, RequestID: requestID, Side: Buy, Price: Price(requestID), Quantity: 1}); err != nil {
			t.Fatal(err)
		}
	}
	book.CloseInput()
	if got := book.Process(8); got != 2 {
		t.Fatalf("first Process=%d, want output capacity 2", got)
	}
	if book.InputDrained() {
		t.Fatal("input drained while a command remains behind output backpressure")
	}
	var results [2]CommandResult
	if got := book.PollResults(results[:]); got != 2 {
		t.Fatalf("PollResults=%d", got)
	}
	if got := book.Process(8); got != 1 || !book.InputDrained() {
		t.Fatalf("final Process=%d drained=%v", got, book.InputDrained())
	}
}

func TestReplayPreservesAcceptedMetadata(t *testing.T) {
	book := newTestBook(t)
	result := book.Replay(
		Command{Kind: SubmitLimit, RequestID: 1, Side: Sell, Price: 100, Quantity: 2},
		ReplayMetadata{OrderID: 42, Sequence: 99, AcceptedAt: 1234},
	)
	if result.Status != Accepted || result.OrderID != 42 || result.Sequence != 99 || result.AcceptedAt != 1234 {
		t.Fatalf("replay result = %+v", result)
	}
	if order := book.orders[42]; order == nil || order.sequence != 99 || order.acceptedAt != 1234 {
		t.Fatalf("replayed order = %+v", order)
	}
}

func assertBookInvariants(t *testing.T, book *OrderBook) {
	t.Helper()
	assertTreeInvariants(t, &book.bids)
	assertTreeInvariants(t, &book.asks)
	orders, levels := 0, 0
	for _, side := range []*sideBook{&book.bids, &book.asks} {
		levels += len(side.levels)
		for _, level := range side.levels {
			var total Quantity
			var previous *order
			for current := level.head; current != nil; current = current.next {
				if current.prev != previous || current.level != level {
					t.Fatalf("broken price-level links at price %d", level.price)
				}
				total += current.remaining
				previous = current
				orders++
			}
			if previous != level.tail || total != level.total {
				t.Fatalf("broken level total/tail at price %d", level.price)
			}
		}
	}
	if orders != book.activeOrders || levels != book.priceLevels || len(book.orders) != orders {
		t.Fatalf("book counts: orders=%d/%d levels=%d/%d map=%d", orders, book.activeOrders, levels, book.priceLevels, len(book.orders))
	}
}
