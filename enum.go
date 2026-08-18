package orderbook

type Side uint8

const (
	Buy Side = iota + 1
	Sell
)

type CommandKind uint8

const (
	SubmitLimit CommandKind = iota + 1
	Cancel
)

type Status uint8

const (
	Accepted Status = iota + 1
	Rejected
	Canceled
	CancelNoop
)

type Reason uint8

const (
	ReasonNone Reason = iota
	InvalidCommand
	InvalidPrice
	InvalidQuantity
	QuantityOverflow
	DuplicateRequestID
	QueueFull
	Closed
	OrderCapacityExceeded
	PriceLevelCapacityExceeded
	UnknownOrder
	FilledOrder
	CanceledOrder
)
