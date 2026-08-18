package orderbook

import "time"

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Config struct {
	InputCapacity    int
	OutputCapacity   int
	MaxActiveOrders  int
	MaxPriceLevels   int
	DefaultBatchSize int
	Clock            Clock
}

func (c Config) validate() error {
	if c.InputCapacity <= 0 || c.OutputCapacity <= 0 || c.MaxActiveOrders <= 0 ||
		c.MaxPriceLevels <= 0 || c.DefaultBatchSize <= 0 {
		return ErrInvalidConfig
	}
	return nil
}
