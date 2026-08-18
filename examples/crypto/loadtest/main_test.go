package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestHarness(t *testing.T) *harness {
	t.Helper()
	h, err := newHarness(config{inputCapacity: "32", outputCapacity: "32", maxOrders: "32", maxLevels: "32", batchSize: "8", trackLatency: true})
	if err != nil {
		t.Fatal(err)
	}
	h.run()
	t.Cleanup(func() { h.closeInput(); h.waitForResults(time.Second); h.stop() })
	return h
}

func postJSON(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	response, err := client.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestHTTPHarnessProcessesOrdersAndExportsMetrics(t *testing.T) {
	h := newTestHarness(t)
	server := httptest.NewServer(h.routes())
	defer server.Close()
	client := server.Client()
	for _, body := range []string{
		`{"request_id":1,"side":"sell","price":6000000,"quantity":10000}`,
		`{"request_id":2,"side":"buy","price":6002000,"quantity":10000}`,
	} {
		response := postJSON(t, client, server.URL+"/orders", body)
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("status=%d", response.StatusCode)
		}
		response.Body.Close()
	}
	response, err := client.Get(server.URL + "/stats?wait_ms=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats["submitted"] != float64(2) || stats["processed_results"] != float64(2) || stats["trades"] != float64(1) || stats["in_flight"] != float64(0) {
		t.Fatalf("stats=%v", stats)
	}
	t.Logf("drained stats=%v", stats)
	metrics, err := client.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Body.Close()
	var text bytes.Buffer
	if _, err := text.ReadFrom(metrics.Body); err != nil {
		t.Fatal(err)
	}
	if metrics.Header.Get("Content-Type") == "" || !bytes.Contains(text.Bytes(), []byte("orderbook_loadtest_trades 1")) {
		t.Fatalf("metrics=%q", text.String())
	}
}

func TestHTTPHarnessCancelsProjectedOrderAndRejectsMalformedInput(t *testing.T) {
	h := newTestHarness(t)
	server := httptest.NewServer(h.routes())
	defer server.Close()
	client := server.Client()
	bad := postJSON(t, client, server.URL+"/orders", `{"request_id":0,"side":"buy","price":1,"quantity":1}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad order status=%d", bad.StatusCode)
	}
	bad.Body.Close()
	order := postJSON(t, client, server.URL+"/orders", `{"request_id":1,"side":"buy","price":5990000,"quantity":10000}`)
	if order.StatusCode != http.StatusAccepted {
		t.Fatalf("order status=%d", order.StatusCode)
	}
	order.Body.Close()
	if !h.waitForResults(time.Second) {
		t.Fatal("order did not drain")
	}
	cancel := postJSON(t, client, server.URL+"/cancels/random", `{"request_id":2}`)
	if cancel.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status=%d", cancel.StatusCode)
	}
	cancel.Body.Close()
	if !h.waitForResults(time.Second) {
		t.Fatal("cancel did not drain")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.metrics.Canceled != 1 || len(h.active) != 0 || h.metrics.BadRequest != 1 {
		t.Fatalf("metrics=%+v active=%d", h.metrics, len(h.active))
	}
	t.Logf("cancel projection metrics=%+v", h.metrics)
}
