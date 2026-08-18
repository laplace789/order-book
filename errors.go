package orderbook

import "errors"

var (
	ErrInvalidConfig = errors.New("orderbook: invalid config")
	ErrQueueFull     = errors.New("orderbook: input queue full")
	ErrClosed        = errors.New("orderbook: input queue closed")
	ErrConsumerOnly  = errors.New("orderbook: Process and PollResults require their respective single consumers")
)
