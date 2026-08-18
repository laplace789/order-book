// Command loadtest exposes a loopback-only HTTP harness for load-testing the
// orderbook package with k6. It is not a production trading API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	orderbook "github.com/laplace789/order-book"
)

type config struct {
	listen, inputCapacity, outputCapacity, maxOrders, maxLevels, batchSize string
	trackLatency                                                           bool
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "HTTP listen address; loopback is safest")
	flag.StringVar(&cfg.inputCapacity, "input-capacity", "65536", "input queue capacity")
	flag.StringVar(&cfg.outputCapacity, "output-capacity", "65536", "output queue capacity")
	flag.StringVar(&cfg.maxOrders, "max-active-orders", "100000", "maximum resting orders")
	flag.StringVar(&cfg.maxLevels, "max-price-levels", "1024", "maximum price levels")
	flag.StringVar(&cfg.batchSize, "batch-size", "4096", "matching worker batch size")
	flag.BoolVar(&cfg.trackLatency, "track-e2e-latency", true, "measure submit-to-result latency")
	flag.Parse()

	h, err := newHarness(cfg)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: h.routes(), ReadHeaderTimeout: 5 * time.Second}
	go h.run()
	go func() {
		log.Printf("k6 harness listening on http://%s", listener.Addr())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals
	log.Printf("stopping ingress and draining accepted commands")
	h.closeInput()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	h.waitForResults(30 * time.Second)
	h.stop()
	log.Printf("final stats: %s", h.statsJSON())
}

type harness struct {
	book         *orderbook.OrderBook
	batchSize    int
	trackLatency bool
	wakeWorker   chan struct{}
	wakeDispatch chan struct{}
	stopCh       chan struct{}
	done         sync.WaitGroup

	inFlight atomic.Int64
	mu       sync.Mutex // protects pending, active, and aggregate metrics.
	pending  map[uint64]pendingCommand
	active   map[uint64]orderbook.Quantity
	metrics  metrics
}

type pendingCommand struct {
	command orderbook.Command
	sentAt  time.Time
}

type metrics struct {
	Submitted, QueueFull, Closed, BadRequest, Processed uint64
	Accepted, Rejected, Canceled, CancelNoop, Trades    uint64
	TradeQuantity                                       uint64
	CancelUnavailable                                   uint64
	latencies                                           []int64
	LatencyCount                                        uint64
	LatencyMax                                          int64
}

func newHarness(cfg config) (*harness, error) {
	parse := func(name, value string) (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return n, nil
	}
	in, err := parse("input-capacity", cfg.inputCapacity)
	if err != nil {
		return nil, err
	}
	out, err := parse("output-capacity", cfg.outputCapacity)
	if err != nil {
		return nil, err
	}
	orders, err := parse("max-active-orders", cfg.maxOrders)
	if err != nil {
		return nil, err
	}
	levels, err := parse("max-price-levels", cfg.maxLevels)
	if err != nil {
		return nil, err
	}
	batch, err := parse("batch-size", cfg.batchSize)
	if err != nil {
		return nil, err
	}
	book, err := orderbook.New(orderbook.Config{InputCapacity: in, OutputCapacity: out, MaxActiveOrders: orders, MaxPriceLevels: levels, DefaultBatchSize: batch})
	if err != nil {
		return nil, err
	}
	return &harness{book: book, batchSize: batch, trackLatency: cfg.trackLatency, wakeWorker: make(chan struct{}, 1), wakeDispatch: make(chan struct{}, 1), stopCh: make(chan struct{}), pending: make(map[uint64]pendingCommand), active: make(map[uint64]orderbook.Quantity), metrics: metrics{latencies: make([]int64, 0, 8192)}}, nil
}

func (h *harness) run()        { h.done.Add(2); go h.worker(); go h.dispatcher() }
func (h *harness) stop()       { close(h.stopCh); h.done.Wait() }
func (h *harness) closeInput() { h.book.CloseInput(); notify(h.wakeWorker) }
func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *harness) worker() {
	defer h.done.Done()
	for {
		if h.book.Process(h.batchSize) > 0 {
			notify(h.wakeDispatch)
			continue
		}
		select {
		case <-h.stopCh:
			return
		case <-h.wakeWorker:
		}
	}
}

func (h *harness) dispatcher() {
	defer h.done.Done()
	results := make([]orderbook.CommandResult, h.batchSize)
	for {
		n := h.book.PollResults(results)
		if n > 0 {
			for _, result := range results[:n] {
				h.observe(result)
			}
			notify(h.wakeWorker)
			continue
		}
		select {
		case <-h.stopCh:
			return
		case <-h.wakeDispatch:
		}
	}
}

func (h *harness) observe(result orderbook.CommandResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, found := h.pending[result.RequestID]
	if found {
		delete(h.pending, result.RequestID)
		h.inFlight.Add(-1)
		if h.trackLatency {
			h.recordLatency(time.Since(pending.sentAt).Nanoseconds())
		}
	}
	h.metrics.Processed++
	switch result.Status {
	case orderbook.Accepted:
		h.metrics.Accepted++
		for _, trade := range result.Trades {
			h.metrics.Trades++
			h.metrics.TradeQuantity += uint64(trade.Quantity)
			if remaining, ok := h.active[trade.MakerOrderID]; ok {
				if remaining <= trade.Quantity {
					delete(h.active, trade.MakerOrderID)
				} else {
					h.active[trade.MakerOrderID] = remaining - trade.Quantity
				}
			}
		}
		if result.Remaining > 0 {
			h.active[result.OrderID] = result.Remaining
		}
	case orderbook.Rejected:
		h.metrics.Rejected++
	case orderbook.Canceled:
		h.metrics.Canceled++
		delete(h.active, result.OrderID)
	case orderbook.CancelNoop:
		h.metrics.CancelNoop++
		if found {
			delete(h.active, pending.command.OrderID)
		}
	}
}

func (h *harness) recordLatency(ns int64) {
	h.metrics.LatencyCount++
	if ns > h.metrics.LatencyMax {
		h.metrics.LatencyMax = ns
	}
	if len(h.metrics.latencies) < cap(h.metrics.latencies) {
		h.metrics.latencies = append(h.metrics.latencies, ns)
		return
	}
	h.metrics.latencies[h.metrics.LatencyCount%uint64(len(h.metrics.latencies))] = ns
}

type orderRequest struct {
	RequestID uint64             `json:"request_id"`
	Side      string             `json:"side"`
	Price     orderbook.Price    `json:"price"`
	Quantity  orderbook.Quantity `json:"quantity"`
}
type cancelRequest struct {
	RequestID uint64 `json:"request_id"`
}

func (h *harness) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", h.submitOrder)
	mux.HandleFunc("POST /cancels/random", h.cancelRandom)
	mux.HandleFunc("GET /stats", h.stats)
	mux.HandleFunc("GET /metrics", h.prometheus)
	return mux
}

func (h *harness) submitOrder(w http.ResponseWriter, r *http.Request) {
	var request orderRequest
	if err := decodeJSON(r, &request); err != nil || request.RequestID == 0 || (request.Side != "buy" && request.Side != "sell") {
		h.recordIngress(func(m *metrics) { m.BadRequest++ })
		http.Error(w, "invalid order request", http.StatusBadRequest)
		return
	}
	side := orderbook.Buy
	if request.Side == "sell" {
		side = orderbook.Sell
	}
	h.submit(w, orderbook.Command{Kind: orderbook.SubmitLimit, RequestID: request.RequestID, Side: side, Price: request.Price, Quantity: request.Quantity})
}

func (h *harness) cancelRandom(w http.ResponseWriter, r *http.Request) {
	var request cancelRequest
	if err := decodeJSON(r, &request); err != nil || request.RequestID == 0 {
		h.recordIngress(func(m *metrics) { m.BadRequest++ })
		http.Error(w, "invalid cancel request", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	var orderID uint64
	for id := range h.active {
		orderID = id
		delete(h.active, id)
		break
	}
	if orderID == 0 {
		h.metrics.CancelUnavailable++
		h.mu.Unlock()
		http.Error(w, "no active order available", http.StatusConflict)
		return
	}
	h.mu.Unlock()
	h.submit(w, orderbook.Command{Kind: orderbook.Cancel, RequestID: request.RequestID, OrderID: orderID})
}

func (h *harness) submit(w http.ResponseWriter, command orderbook.Command) {
	h.mu.Lock()
	if _, exists := h.pending[command.RequestID]; exists {
		h.metrics.BadRequest++
		h.mu.Unlock()
		http.Error(w, "duplicate in-flight request_id", http.StatusConflict)
		return
	}
	h.pending[command.RequestID] = pendingCommand{command: command, sentAt: time.Now()}
	h.mu.Unlock()
	if err := h.book.Submit(command); err != nil {
		h.mu.Lock()
		delete(h.pending, command.RequestID)
		if errors.Is(err, orderbook.ErrQueueFull) {
			h.metrics.QueueFull++
		} else {
			h.metrics.Closed++
		}
		h.mu.Unlock()
		if errors.Is(err, orderbook.ErrQueueFull) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		} else {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
		return
	}
	h.inFlight.Add(1)
	h.recordIngress(func(m *metrics) { m.Submitted++ })
	notify(h.wakeWorker)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]uint64{"request_id": command.RequestID})
}

func (h *harness) recordIngress(update func(*metrics)) {
	h.mu.Lock()
	update(&h.metrics)
	h.mu.Unlock()
}
func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// waitForResults waits for all commands accepted so far. It does not inspect
// the MPSC queue, whose consumer-owned state must not be read concurrently.
func (h *harness) waitForResults(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.inFlight.Load() == 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (h *harness) stats(w http.ResponseWriter, r *http.Request) {
	if ms, _ := strconv.Atoi(r.URL.Query().Get("wait_ms")); ms > 0 {
		h.waitForResults(time.Duration(ms) * time.Millisecond)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(h.statsJSON()))
}

func (h *harness) statsJSON() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	latencies := append([]int64(nil), h.metrics.latencies...)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	quantile := func(q float64) int64 {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)]
	}
	result := map[string]any{"submitted": h.metrics.Submitted, "queue_full": h.metrics.QueueFull, "closed": h.metrics.Closed, "bad_request": h.metrics.BadRequest, "processed_results": h.metrics.Processed, "accepted": h.metrics.Accepted, "rejected": h.metrics.Rejected, "canceled": h.metrics.Canceled, "cancel_noop": h.metrics.CancelNoop, "cancel_unavailable": h.metrics.CancelUnavailable, "trades": h.metrics.Trades, "trade_quantity_lots": h.metrics.TradeQuantity, "in_flight": h.inFlight.Load(), "results_drained": h.inFlight.Load() == 0, "active_projection": len(h.active), "e2e_latency_ns": map[string]any{"count": h.metrics.LatencyCount, "p50": quantile(.50), "p95": quantile(.95), "p99": quantile(.99), "max": h.metrics.LatencyMax}}
	b, _ := json.Marshal(result)
	return string(b)
}

func (h *harness) prometheus(w http.ResponseWriter, _ *http.Request) {
	var data map[string]any
	_ = json.Unmarshal([]byte(h.statsJSON()), &data)
	var out strings.Builder
	for _, key := range []string{"submitted", "queue_full", "closed", "bad_request", "processed_results", "accepted", "rejected", "canceled", "cancel_noop", "cancel_unavailable", "trades", "trade_quantity_lots", "in_flight", "active_projection"} {
		fmt.Fprintf(&out, "orderbook_loadtest_%s %v\n", key, data[key])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, out.String())
}
