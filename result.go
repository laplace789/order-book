package orderbook

type Trade struct {
	MakerOrderID uint64
	TakerOrderID uint64
	Price        Price
	Quantity     Quantity
}

type CommandResult struct {
	RequestID  uint64
	Status     Status
	Reason     Reason
	OrderID    uint64
	Sequence   uint64
	AcceptedAt int64
	Remaining  Quantity
	Trades     []Trade
}
