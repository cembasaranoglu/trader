package binance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// twoHostClient serves the signed and market-data hosts from separate servers.
func twoHostClient(t *testing.T, signed, market http.Handler, clk *fakeClock) *Client {
	t.Helper()
	s1, s2 := httptest.NewServer(signed), httptest.NewServer(market)
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)
	c, err := New(Options{BaseURL: s1.URL, MarketDataURL: s2.URL, HTTPClient: s1.Client(), Credentials: hmacCreds(t), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBackoffIsPerHost(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var signedHits atomic.Int32
	c := twoHostClient(t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			signedHits.Add(1)
			fmt.Fprint(w, `{"serverTime":1700000000000}`)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTeapot)
		}), clk)
	if _, err := c.Tickers24h(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want rate limit from market host, got %v", err)
	}
	if _, err := c.Tickers24h(context.Background()); !errors.Is(err, ErrBackoff) {
		t.Fatalf("market host must be in backoff, got %v", err)
	}
	// A market-data ban must not block the signed host (cancels, protection).
	if _, err := c.ServerTime(context.Background()); err != nil || signedHits.Load() != 1 {
		t.Fatalf("signed host blocked by market-data backoff: %v", err)
	}
	if !c.BlockedUntilHost(c.SignedHost()).IsZero() || c.BlockedUntilHost(c.MarketDataHost()).IsZero() {
		t.Fatal("backoff recorded on the wrong host")
	}
}

func TestMarketWeightBudget(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var used atomic.Int64
	used.Store(10)
	var depthHits atomic.Int32
	c := twoHostClient(t, http.NotFoundHandler(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", strconv.FormatInt(used.Load(), 10))
		switch r.URL.Path {
		case "/api/v3/exchangeInfo":
			w.Write(fixture(t, "exchange_info.json"))
		case "/api/v3/depth":
			depthHits.Add(1)
			fmt.Fprint(w, `{"lastUpdateId":1,"bids":[],"asks":[]}`)
		}
	}), clk)
	if _, err := c.Symbols(context.Background()); err != nil {
		t.Fatal(err)
	}
	// exchange_info.json declares REQUEST_WEIGHT 6000/1m → budget 4800.
	used.Store(4790)
	if _, err := c.Depth(context.Background(), sym("BTCUSDT"), 5); err != nil { // records 4790
		t.Fatal(err)
	}
	if _, err := c.Depth(context.Background(), sym("BTCUSDT"), 1000); !errors.Is(err, ErrWeightBudget) {
		t.Fatalf("4790 + 50 > 4800 must be refused locally, got %v", err)
	}
	if depthHits.Load() != 1 {
		t.Fatalf("refused request reached the server: hits %d", depthHits.Load())
	}
	if _, err := c.Depth(context.Background(), sym("BTCUSDT"), 5); err != nil {
		t.Fatalf("4790 + 5 <= 4800 must pass: %v", err)
	}
	clk.Advance(usedWeightTTL)
	used.Store(0)
	if _, err := c.Depth(context.Background(), sym("BTCUSDT"), 1000); err != nil {
		t.Fatalf("stale reading must not block: %v", err)
	}
}

func TestDepthWeight(t *testing.T) {
	t.Parallel()
	for limit, want := range map[int]int{1: 5, 100: 5, 101: 25, 500: 25, 501: 50, 1000: 50, 1001: 250, 5000: 250} {
		if got := DepthWeight(limit); got != want {
			t.Errorf("DepthWeight(%d) = %d, want %d", limit, got, want)
		}
	}
}

func TestKlinesRange(t *testing.T) {
	t.Parallel()
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write(fixture(t, "klines.json"))
	}), nil, nil)
	start, end := time.UnixMilli(1700000000000), time.UnixMilli(1700000060000)
	ks, err := c.KlinesRange(context.Background(), sym("BTCUSDT"), "1m", start, end, 2)
	if err != nil || len(ks) != 1 {
		t.Fatalf("%v %v", ks, err)
	}
	if gotQuery != "endTime=1700000060000&interval=1m&limit=2&startTime=1700000000000&symbol=BTCUSDT" {
		t.Fatalf("query %s", gotQuery)
	}
	for name, call := range map[string]func() error{
		"end before start": func() error { _, err := c.KlinesRange(context.Background(), sym("X"), "1m", end, start, 2); return err },
		"zero start": func() error {
			_, err := c.KlinesRange(context.Background(), sym("X"), "1m", time.Time{}, end, 2)
			return err
		},
		"bad interval": func() error { _, err := c.KlinesRange(context.Background(), sym("X"), "2m", start, end, 2); return err },
		"bad limit": func() error {
			_, err := c.KlinesRange(context.Background(), sym("X"), "1m", start, end, 1001)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
