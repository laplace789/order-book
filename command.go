package orderbook

// Command is immutable after Submit returns true.
type Command struct {
	Kind      CommandKind
	RequestID uint64
	Side      Side
	Price     Price
	Quantity  Quantity
	OrderID   uint64
}

// ReplayMetadata preserves identifiers and acceptance time from durable event
// data. It is only valid for replaying an accepted SubmitLimit command.
type ReplayMetadata struct {
	OrderID    uint64
	Sequence   uint64
	AcceptedAt int64
}
