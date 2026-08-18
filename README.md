# order-book

單一 market 的 Go 連續撮合 order book 核心。它只處理記憶體內的限價單撮合與撤單；不處理帳戶、餘額、風控、WAL、symbol routing 或對外行情分發。

目前支援：

- GTC limit order
- 依 engine OrderID 取消
- 價格時間優先與 maker-price 成交
- 多 producer、單撮合 worker 的 MPSC 輸入
- 單 producer、單 consumer 的有界結果輸出與背壓
- 價格 map、intrusive red-black tree、同價 FIFO doubly linked list
- deterministic replay metadata
- per-book free list 回收 order / price-level 節點

## 核心資料流

```text
many producers
    │ Submit(Command)
    ▼
MPSC input queue
    │ PollVec
    ▼
matching worker / Process
    │
    ├─ bid / ask price levels
    │  ├─ map[Price]*PriceLevel
    │  ├─ intrusive red-black tree
    │  └─ FIFO doubly linked list of orders
    │
    ▼
bounded result queue
    │ PollResults
    ▼
one event dispatcher
```

撮合只會在唯一 matching worker 呼叫 Process(maxBatch) 時發生。成功 Submit 只代表 command 已進 input queue，不代表已成交。

例如 book 中已有 buy 100，新 sell 100 被處理時，sell 會比對 best bid 100，因 100 <= 100 而成交。原本的 buy 是 maker，成交價為 maker 的 100；同價多筆 buy 依 FIFO 順序成交。

## 使用模型

```go
book, err := orderbook.New(orderbook.Config{
    InputCapacity: 1024, OutputCapacity: 1024,
    MaxActiveOrders: 100_000, MaxPriceLevels: 10_000,
    DefaultBatchSize: 128,
})
if err != nil { panic(err) }

// 可由多個 producer goroutine 同時呼叫。
err = book.Submit(orderbook.Command{
    Kind: orderbook.SubmitLimit, RequestID: 1,
    Side: orderbook.Buy, Price: 100, Quantity: 2,
})

// 唯一 matching worker 呼叫。
book.Process(128)

// 唯一 event dispatcher 呼叫。
results := make([]orderbook.CommandResult, 128)
n := book.PollResults(results)
_ = n
```

價格與數量是整數：Price 為交易對設定的價格 tick 數，Quantity 為 base-asset lot 數。decimal 轉換必須在 order-book 外完成；熱路徑不使用 float64 或 decimal。

## K6 load test

[`examples/crypto/loadtest`](examples/crypto/loadtest/README.md) 是僅供壓測的 loopback HTTP harness。它用 K6 測量多 producer ingress、單 matching worker、單 result dispatcher 與 output backpressure；並提供同價等量 buy/sell 的功能性壓測，驗證高併發下所有成功入隊的訂單都能產生結果且最終沒有殘單。

```sh
go run ./examples/crypto/loadtest
k6 run examples/crypto/loadtest/k6/crypto.js
PROFILE=same-price-2000 k6 run examples/crypto/loadtest/k6/same-price.js
```

完整設定、metrics 與驗收條件請見該目錄的 README。這個 harness 不包含帳戶、餘額、費率、結算或任何 production HTTP API 功能。

## 背壓與並發契約

- Submit 滿載或關閉時立即回傳錯誤，不阻塞。
- 只有一個 goroutine 可呼叫 Process。
- 只有一個 goroutine 可呼叫 PollResults。
- output queue 滿時，Process 會停止處理更多 command，且不遺失結果。
- MPSC queue 無 mutex；但它不宣稱形式上的 lock-free progress，因 producer 在預留 slot 後被暫停時，consumer 無法跳過該 FIFO slot。

完整行為契約請見 [SPEC.md](SPEC.md)。

## 開發與驗證

需要 Go 1.25.8。

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem
go test -run '^$' -fuzz=FuzzTreeAgainstMap -fuzztime=10s
```

紅黑樹測試包含 invariant 驗證、經典旋轉／刪除情境、100,000 步 map 差分測試與 fuzz。
