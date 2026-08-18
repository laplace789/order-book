package orderbook

import (
	"testing"
	"time"
)

func BenchmarkCrossingOrders(b *testing.B) {
	book, err := New(Config{
		InputCapacity: 1024, OutputCapacity: 1024, MaxActiveOrders: 1024, MaxPriceLevels: 1024,
		DefaultBatchSize: 1, Clock: fixedClock{now: time.Unix(0, 0)},
	})
	if err != nil {
		b.Fatal(err)
	}
	var result [1]CommandResult
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := uint64(i*2 + 1)
		if err := book.Submit(Command{Kind: SubmitLimit, RequestID: base, Side: Sell, Price: 100, Quantity: 1}); err != nil {
			b.Fatal(err)
		}
		book.Process(1)
		book.PollResults(result[:])
		if err := book.Submit(Command{Kind: SubmitLimit, RequestID: base + 1, Side: Buy, Price: 100, Quantity: 1}); err != nil {
			b.Fatal(err)
		}
		book.Process(1)
		book.PollResults(result[:])
	}
}
