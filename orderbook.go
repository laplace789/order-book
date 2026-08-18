package orderbook

import (
	"sync/atomic"

	mpsc "github.com/laplace789/mpsc-ringbuffer"
)

// Book is safe for concurrent Submit calls. Process and PollResults each have
// exactly one designated caller.
type Book interface {
	Submit(Command) error
	Process(maxBatch int) int
	PollResults(dst []CommandResult) int
	Replay(Command, ReplayMetadata) CommandResult
	CloseInput()
	InputDrained() bool
}

type OrderBook struct {
	input  mpsc.RingBuffer[Command]
	output mpsc.RingBuffer[CommandResult]

	cfg Config

	bids sideBook
	asks sideBook

	orders     map[uint64]*order
	requestIDs map[uint64]struct{}
	terminal   map[uint64]Reason

	nextOrderID  uint64
	nextSequence uint64
	activeOrders int
	priceLevels  int

	orderFree *order
	levelFree *priceLevel

	outputReserved atomic.Int64
	processBuffer  []Command
}

func New(cfg Config) (*OrderBook, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	input, err := mpsc.New[Command](mpsc.RingBufferTypeSimple, cfg.InputCapacity)
	if err != nil {
		return nil, err
	}
	output, err := mpsc.New[CommandResult](mpsc.RingBufferTypeSimple, cfg.OutputCapacity)
	if err != nil {
		return nil, err
	}
	book := &OrderBook{
		input:         input,
		output:        output,
		cfg:           cfg,
		orders:        make(map[uint64]*order, cfg.MaxActiveOrders),
		requestIDs:    make(map[uint64]struct{}, cfg.MaxActiveOrders),
		terminal:      make(map[uint64]Reason, cfg.MaxActiveOrders),
		processBuffer: make([]Command, cfg.DefaultBatchSize),
	}
	book.bids.init(Buy)
	book.asks.init(Sell)
	return book, nil
}

func (b *OrderBook) Submit(command Command) error {
	if b.input.IsClosed() {
		return ErrClosed
	}
	if !b.input.Offer(command) {
		if b.input.IsClosed() {
			return ErrClosed
		}
		return ErrQueueFull
	}
	return nil
}

func (b *OrderBook) CloseInput() { b.input.Close() }

func (b *OrderBook) InputDrained() bool { return b.input.IsDrained() }

func (b *OrderBook) reserveOutputs(maximum int) int {
	for {
		current := b.outputReserved.Load()
		available := int64(b.output.Capacity()) - current
		if available <= 0 {
			return 0
		}
		reserved := int64(maximum)
		if reserved > available {
			reserved = available
		}
		if b.outputReserved.CompareAndSwap(current, current+reserved) {
			return int(reserved)
		}
	}
}

func (b *OrderBook) Process(maxBatch int) int {
	if maxBatch <= 0 || maxBatch > len(b.processBuffer) {
		maxBatch = b.cfg.DefaultBatchSize
	}
	reserved := b.reserveOutputs(maxBatch)
	if reserved == 0 {
		return 0
	}
	processed := b.input.PollVec(b.processBuffer[:reserved])
	if processed != reserved {
		b.outputReserved.Add(-int64(reserved - processed))
	}
	for _, command := range b.processBuffer[:processed] {
		result := b.apply(command)
		if !b.output.Offer(result) {
			panic("orderbook: output reservation and queue diverged")
		}
	}
	return processed
}

func (b *OrderBook) PollResults(dst []CommandResult) int {
	if len(dst) == 0 {
		return 0
	}
	n := b.output.PollVec(dst)
	if n > 0 {
		b.outputReserved.Add(-int64(n))
	}
	return n
}

// Replay applies a previously accepted command without queueing it or emitting
// an output event. The caller must use the same single-writer contract as
// Process. Metadata is used only for SubmitLimit; cancel commands ignore it.
func (b *OrderBook) Replay(command Command, metadata ReplayMetadata) CommandResult {
	result := CommandResult{RequestID: command.RequestID}
	if command.RequestID == 0 {
		result.Status, result.Reason = Rejected, InvalidCommand
		return result
	}
	if _, exists := b.requestIDs[command.RequestID]; exists {
		result.Status, result.Reason = Rejected, DuplicateRequestID
		return result
	}
	b.requestIDs[command.RequestID] = struct{}{}
	if command.Kind == SubmitLimit {
		return b.submitLimitReplay(command, result, metadata)
	}
	if command.Kind == Cancel {
		return b.cancel(command, result)
	}
	result.Status, result.Reason = Rejected, InvalidCommand
	return result
}

func (b *OrderBook) apply(command Command) CommandResult {
	result := CommandResult{RequestID: command.RequestID}
	if command.RequestID == 0 {
		result.Status, result.Reason = Rejected, InvalidCommand
		return result
	}
	if _, exists := b.requestIDs[command.RequestID]; exists {
		result.Status, result.Reason = Rejected, DuplicateRequestID
		return result
	}
	b.requestIDs[command.RequestID] = struct{}{}

	switch command.Kind {
	case SubmitLimit:
		return b.submitLimit(command, result)
	case Cancel:
		return b.cancel(command, result)
	default:
		result.Status, result.Reason = Rejected, InvalidCommand
		return result
	}
}

func (b *OrderBook) submitLimit(command Command, result CommandResult) CommandResult {
	return b.submitLimitWithMetadata(command, result, ReplayMetadata{})
}

func (b *OrderBook) submitLimitReplay(command Command, result CommandResult, metadata ReplayMetadata) CommandResult {
	if metadata.OrderID == 0 || metadata.Sequence == 0 || metadata.OrderID <= b.nextOrderID || metadata.Sequence <= b.nextSequence {
		result.Status, result.Reason = Rejected, InvalidCommand
		return result
	}
	previousOrderID, previousSequence := b.nextOrderID, b.nextSequence
	b.nextOrderID = metadata.OrderID
	b.nextSequence = metadata.Sequence
	result = b.submitLimitWithMetadata(command, result, metadata)
	if result.Status != Accepted {
		b.nextOrderID, b.nextSequence = previousOrderID, previousSequence
	}
	return result
}

func (b *OrderBook) submitLimitWithMetadata(command Command, result CommandResult, metadata ReplayMetadata) CommandResult {
	if command.Side != Buy && command.Side != Sell {
		result.Status, result.Reason = Rejected, InvalidCommand
		return result
	}
	if command.Price <= 0 {
		result.Status, result.Reason = Rejected, InvalidPrice
		return result
	}
	if command.Quantity == 0 {
		result.Status, result.Reason = Rejected, InvalidQuantity
		return result
	}

	own := b.side(command.Side)
	// Conservative admission prevents a capacity error after any fills have
	// mutated the book. Matching may later free capacity, but this command is
	// still rejected if it could require a new resting order or price level.
	if b.activeOrders >= b.cfg.MaxActiveOrders {
		result.Status, result.Reason = Rejected, OrderCapacityExceeded
		return result
	}
	if own.levels[command.Price] == nil && b.priceLevels >= b.cfg.MaxPriceLevels {
		result.Status, result.Reason = Rejected, PriceLevelCapacityExceeded
		return result
	}
	if level := own.levels[command.Price]; level != nil && ^Quantity(0)-level.total < command.Quantity {
		result.Status, result.Reason = Rejected, QuantityOverflow
		return result
	}
	if metadata.OrderID == 0 {
		b.nextOrderID++
		b.nextSequence++
		metadata = ReplayMetadata{
			OrderID: b.nextOrderID, Sequence: b.nextSequence, AcceptedAt: b.cfg.Clock.Now().UTC().UnixNano(),
		}
	}

	incoming := b.acquireOrder()
	incoming.id = metadata.OrderID
	incoming.requestID = command.RequestID
	incoming.side = command.Side
	incoming.price = command.Price
	incoming.remaining = command.Quantity
	incoming.sequence = metadata.Sequence
	incoming.acceptedAt = metadata.AcceptedAt

	result.Status = Accepted
	result.OrderID = incoming.id
	result.Sequence = incoming.sequence
	result.AcceptedAt = incoming.acceptedAt

	b.match(incoming, &result)
	if incoming.remaining != 0 {
		if !b.addResting(incoming) {
			panic("orderbook: admission precheck diverged")
		}
	} else {
		b.terminal[incoming.id] = FilledOrder
		b.releaseOrder(incoming)
	}
	result.Remaining = incoming.remaining
	return result
}

func (b *OrderBook) cancel(command Command, result CommandResult) CommandResult {
	if command.OrderID == 0 {
		result.Status, result.Reason = CancelNoop, UnknownOrder
		return result
	}
	order := b.orders[command.OrderID]
	if order == nil {
		result.Status = CancelNoop
		if reason, found := b.terminal[command.OrderID]; found {
			result.Reason = reason
		} else {
			result.Reason = UnknownOrder
		}
		return result
	}
	result.Status = Canceled
	result.OrderID = order.id
	result.Remaining = order.remaining
	b.removeResting(order)
	b.terminal[order.id] = CanceledOrder
	b.releaseOrder(order)
	return result
}

func (b *OrderBook) side(direction Side) *sideBook {
	if direction == Buy {
		return &b.bids
	}
	return &b.asks
}

func (b *OrderBook) match(incoming *order, result *CommandResult) {
	opposite := b.side(oppositeSide(incoming.side))
	for incoming.remaining > 0 {
		level := opposite.best
		if level == nil || !crosses(incoming.side, incoming.price, level.price) {
			return
		}
		maker := level.head
		fill := incoming.remaining
		if maker.remaining < fill {
			fill = maker.remaining
		}
		result.Trades = append(result.Trades, Trade{
			MakerOrderID: maker.id,
			TakerOrderID: incoming.id,
			Price:        level.price,
			Quantity:     fill,
		})
		incoming.remaining -= fill
		maker.remaining -= fill
		level.total -= fill
		if maker.remaining == 0 {
			b.removeResting(maker)
			b.terminal[maker.id] = FilledOrder
			b.releaseOrder(maker)
		}
	}
}

func crosses(side Side, price Price, oppositePrice Price) bool {
	if side == Buy {
		return price >= oppositePrice
	}
	return price <= oppositePrice
}

func oppositeSide(side Side) Side {
	if side == Buy {
		return Sell
	}
	return Buy
}

func (b *OrderBook) addResting(o *order) bool {
	side := b.side(o.side)
	level := side.levels[o.price]
	if level == nil {
		if b.priceLevels >= b.cfg.MaxPriceLevels {
			return false
		}
		level = b.acquireLevel(o.price)
		if !side.insertLevel(level) {
			b.releaseLevel(level)
			return false
		}
		b.priceLevels++
	}
	if ^Quantity(0)-level.total < o.remaining {
		return false
	}
	o.level = level
	o.prev, o.next = level.tail, nil
	if level.tail != nil {
		level.tail.next = o
	} else {
		level.head = o
	}
	level.tail = o
	level.total += o.remaining
	b.orders[o.id] = o
	b.activeOrders++
	return true
}

func (b *OrderBook) removeResting(o *order) {
	level := o.level
	if o.prev != nil {
		o.prev.next = o.next
	} else {
		level.head = o.next
	}
	if o.next != nil {
		o.next.prev = o.prev
	} else {
		level.tail = o.prev
	}
	level.total -= o.remaining
	delete(b.orders, o.id)
	b.activeOrders--
	o.prev, o.next, o.level = nil, nil, nil
	if level.head == nil {
		side := b.side(o.side)
		if !side.removeLevel(level) {
			panic("orderbook: active price level missing from tree")
		}
		b.priceLevels--
		b.releaseLevel(level)
	}
}

func (b *OrderBook) acquireOrder() *order {
	if b.orderFree == nil {
		return &order{}
	}
	o := b.orderFree
	b.orderFree = o.freeNext
	*o = order{}
	return o
}

func (b *OrderBook) releaseOrder(o *order) {
	*o = order{}
	o.freeNext = b.orderFree
	b.orderFree = o
}

func (b *OrderBook) acquireLevel(price Price) *priceLevel {
	if b.levelFree == nil {
		return &priceLevel{price: price}
	}
	level := b.levelFree
	b.levelFree = level.freeNext
	*level = priceLevel{price: price}
	return level
}

func (b *OrderBook) releaseLevel(level *priceLevel) {
	*level = priceLevel{}
	level.freeNext = b.levelFree
	b.levelFree = level
}
