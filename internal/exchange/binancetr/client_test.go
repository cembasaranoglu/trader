package binancetr

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newTestClient serves every host from one httptest server.
func newTestClient(t *testing.T, h http.Handler, creds *Credentials, clk *fakeClock) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts := Options{
		BaseURL:       srv.URL + "/base",
		MainMarketURL: srv.URL + "/main",
		NextMarketURL: srv.URL + "/next",
		HTTPClient:    srv.Client(),
		Credentials:   creds,
	}
	if clk != nil {
		opts.Now = clk.Now
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := exchange.ParseDecimal(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func eqRat(a, b *big.Rat) bool { return a != nil && b != nil && a.Cmp(b) == 0 }

func TestSignKnownVector(t *testing.T) {
	t.Parallel()
	// Published HMAC-SHA256 example from the Binance spot API documentation.
	got := sign("NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j",
		"symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559")
	if want := "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts Options
	}{
		{"bad base url", Options{BaseURL: "not a url"}},
		{"key without secret", Options{Credentials: &Credentials{APIKey: "k"}}},
		{"recv window too large", Options{RecvWindow: 61 * time.Second}},
		{"negative recv window", Options{RecvWindow: -time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.opts); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := New(Options{}); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
}

func TestServerTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr bool
	}{
		{"envelope", `{"code":0,"msg":"success","timestamp":1572265137927}`, 1572265137927, false},
		{"raw", `{"serverTime":1572265137928}`, 1572265137928, false},
		{"missing", `{"foo":1}`, 0, true},
		{"non-zero code", `{"code":-1,"msg":"nope","timestamp":1}`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/base/open/v1/common/time" {
					t.Errorf("path %s", r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			}), nil, nil)
			got, err := c.ServerTime(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.UnixMilli() != tc.want {
				t.Fatalf("got %d, want %d", got.UnixMilli(), tc.want)
			}
		})
	}
}

func TestSymbols(t *testing.T) {
	t.Parallel()
	body := fixture(t, "symbols.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}), nil, nil)
	syms, err := c.Symbols(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 2 {
		t.Fatalf("got %d symbols", len(syms))
	}
	btc := syms[0]
	if btc.Name != "BTC_TRY" || btc.Quote != "TRY" || btc.Type != 1 || !btc.SpotEnabled || !btc.OCOEnabled {
		t.Fatalf("unexpected symbol: %+v", btc)
	}
	f := btc.Filters
	if !eqRat(f.TickSize, rat(t, "1")) || !eqRat(f.StepSize, rat(t, "0.00001")) || !eqRat(f.MinNotional, rat(t, "100")) || !f.NotionalAppliesToMarket {
		t.Fatalf("unexpected filters: %+v", f)
	}
	if f.MinQty == nil || !eqRat(f.MinQty, rat(t, "0.00001")) {
		t.Fatal("LOT_SIZE minQty must win over MARKET_LOT_SIZE")
	}
	next := syms[1]
	if next.SpotEnabled || next.OCOEnabled || !eqRat(next.Filters.MinNotional, rat(t, "50")) {
		t.Fatalf("legacy MIN_NOTIONAL and disabled flags not parsed: %+v", next)
	}
}

func TestSymbolsRejectsBadDecimal(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"list":[{"type":1,"symbol":"X_TRY","filters":[{"filterType":"PRICE_FILTER","tickSize":"abc"}]}]}}`)
	}), nil, nil)
	if _, err := c.Symbols(context.Background()); err == nil || !strings.Contains(err.Error(), "X_TRY") {
		t.Fatalf("want error naming the symbol, got %v", err)
	}
}

func TestDepthRoutingAndShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sym        exchange.Symbol
		body       string
		wantPath   string
		wantSymbol string
		wantShape  Shape
	}{
		{"main host envelope", exchange.Symbol{Name: "BTC_TRY", Type: 1}, "depth_envelope.json", "/main/api/v3/depth", "BTCTRY", ShapeEnvelope},
		{"main host raw", exchange.Symbol{Name: "BTC_TRY", Type: 1}, "depth_raw.json", "/main/api/v3/depth", "BTCTRY", ShapeRaw},
		{"next host keeps underscore", exchange.Symbol{Name: "NEW_TRY", Type: 2}, "depth_envelope.json", "/next/api/v1/depth", "NEW_TRY", ShapeEnvelope},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, tc.body)
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath || r.URL.Query().Get("symbol") != tc.wantSymbol || r.URL.Query().Get("limit") != "5" {
					t.Errorf("request %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				w.Write(body)
			}), nil, nil)
			ob, shape, err := c.Depth(context.Background(), tc.sym, 5)
			if err != nil {
				t.Fatal(err)
			}
			if shape != tc.wantShape {
				t.Errorf("shape %s, want %s", shape, tc.wantShape)
			}
			if ob.LastUpdateID != 1027024 || len(ob.Bids) != 1 || !eqRat(ob.Asks[0].Price, rat(t, "4.000002")) {
				t.Fatalf("unexpected book: %+v", ob)
			}
		})
	}
}

func TestDepthLimitValidatedLocally(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), nil, nil)
	if _, _, err := c.Depth(context.Background(), exchange.Symbol{Name: "BTC_TRY", Type: 1}, 7); err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 0 {
		t.Fatal("invalid limit must not reach the server")
	}
}

func TestKlines(t *testing.T) {
	t.Parallel()
	body := fixture(t, "klines.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/main/api/v1/klines" || r.URL.Query().Get("interval") != "1h" {
			t.Errorf("request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Write(body)
	}), nil, nil)
	ks, shape, err := c.Klines(context.Background(), exchange.Symbol{Name: "BTC_TRY", Type: 1}, "1h", 1)
	if err != nil {
		t.Fatal(err)
	}
	if shape != ShapeEnvelope || len(ks) != 1 {
		t.Fatalf("shape %s, len %d", shape, len(ks))
	}
	k := ks[0]
	if k.OpenTime.UnixMilli() != 1499040000000 || k.CloseTime.UnixMilli() != 1499644799999 || k.Trades != 308 ||
		!eqRat(k.Close, rat(t, "0.015771")) || !eqRat(k.QuoteVol, rat(t, "2434.19055334")) {
		t.Fatalf("unexpected kline: %+v", k)
	}
	for _, bad := range []int{0, 1001} {
		if _, _, err := c.Klines(context.Background(), exchange.Symbol{Type: 1}, "1h", bad); err == nil {
			t.Errorf("limit %d must be rejected", bad)
		}
	}
}

func TestKlinesMalformedRow(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":[[1,"1","2"]]}`)
	}), nil, nil)
	if _, _, err := c.Klines(context.Background(), exchange.Symbol{Type: 1}, "1h", 1); err == nil {
		t.Fatal("expected error for short row")
	}
}

func TestAccountSignsRequest(t *testing.T) {
	t.Parallel()
	creds := &Credentials{APIKey: "test-key", SecretKey: "test-secret"}
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	body := fixture(t, "account.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-MBX-APIKEY") != "test-key" {
			t.Errorf("missing api key header")
		}
		raw := r.URL.RawQuery
		i := strings.LastIndex(raw, "&signature=")
		if i < 0 {
			t.Fatalf("no signature in %q", raw)
		}
		payload, sig := raw[:i], raw[i+len("&signature="):]
		if sig != sign("test-secret", payload) {
			t.Errorf("bad signature for %q", payload)
		}
		q, _ := url.ParseQuery(payload)
		if q.Get("timestamp") != "1700000000000" || q.Get("recvWindow") != "5000" {
			t.Errorf("timestamp/recvWindow: %v", q)
		}
		w.Write(body)
	}), creds, clk)
	info, err := c.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.CanTrade || !info.CanWithdraw || !eqRat(info.FiatTakerRate, rat(t, "0.0015")) || info.RawMakerCommission != "10.00000000" {
		t.Fatalf("unexpected account: %+v", info)
	}
	if len(info.Balances) != 2 || !eqRat(info.Balances[1].Locked, rat(t, "0.0001")) {
		t.Fatalf("unexpected balances: %+v", info.Balances)
	}
}

func TestAccountWithoutCredentials(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), nil, nil)
	if _, err := c.Account(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("must not call the server without credentials")
	}
}

func TestRateLimitBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		retryAfter string
		wantWait   time.Duration
	}{
		{"429 with header", http.StatusTooManyRequests, "5", 5 * time.Second},
		{"418 without header uses default", http.StatusTeapot, "", defaultRetryAfter},
		{"429 malformed header uses default", http.StatusTooManyRequests, "soon", defaultRetryAfter},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := &fakeClock{t: time.Unix(1700000000, 0)}
			var hits atomic.Int32
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					w.WriteHeader(tc.status)
					return
				}
				fmt.Fprint(w, `{"code":0,"timestamp":1}`)
			}), nil, clk)

			_, err := c.ServerTime(context.Background())
			var rl *RateLimitError
			if !errors.As(err, &rl) || rl.Status != tc.status || rl.RetryAfter != tc.wantWait {
				t.Fatalf("want RateLimitError{%d,%s}, got %v", tc.status, tc.wantWait, err)
			}
			if !errors.Is(err, ErrRateLimited) {
				t.Fatal("must wrap ErrRateLimited")
			}
			if _, err := c.ServerTime(context.Background()); !errors.Is(err, ErrBackoff) {
				t.Fatalf("want ErrBackoff during window, got %v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("backoff must not contact the server, hits=%d", hits.Load())
			}
			clk.Advance(tc.wantWait)
			if _, err := c.ServerTime(context.Background()); err != nil {
				t.Fatalf("after window: %v", err)
			}
		})
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   int
		body     string
		sentinel error
		wantCode int
	}{
		{"server error", http.StatusServiceUnavailable, "", ErrServer, 0},
		{"waf forbidden", http.StatusForbidden, "blocked", ErrAPI, 0},
		{"bad request with envelope", http.StatusBadRequest, `{"code":-1121,"msg":"Invalid symbol."}`, ErrAPI, -1121},
		{"200 with error code", http.StatusOK, `{"code":3210,"message":"param error"}`, ErrAPI, 3210},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}), nil, nil)
			_, err := c.ServerTime(context.Background())
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("want %v, got %v", tc.sentinel, err)
			}
			var api *APIError
			if errors.As(err, &api) && api.Code != tc.wantCode {
				t.Fatalf("code %d, want %d", api.Code, tc.wantCode)
			}
		})
	}
}

func TestUsageHeadersRecorded(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "12")
		w.Header().Set("X-Unrelated", "x")
		fmt.Fprint(w, `{"code":0,"timestamp":1}`)
	}), nil, nil)
	if _, err := c.ServerTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := c.Usage()
	if u["x-mbx-used-weight-1m"] != "12" || len(u) != 1 {
		t.Fatalf("usage %v", u)
	}
}

func TestResponseTooLarge(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat(" ", MaxBodyBytes+1)))
	}), nil, nil)
	if _, err := c.ServerTime(context.Background()); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestContextCanceled(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ServerTime(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSecretsNeverPrinted(t *testing.T) {
	t.Parallel()
	creds := Credentials{APIKey: "visible-key-123", SecretKey: "super-secret-456"}
	for _, s := range []string{fmt.Sprintf("%v", creds), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds), fmt.Sprintf("%v", &creds)} {
		if strings.Contains(s, "super-secret-456") || strings.Contains(s, "visible-key-123") {
			t.Fatalf("credentials leaked: %s", s)
		}
	}

	// Transport errors must not echo the signed query string.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Credentials: &creds})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Account(context.Background())
	if err == nil || strings.Contains(err.Error(), "signature=") || strings.Contains(err.Error(), "super-secret-456") {
		t.Fatalf("unexpected or leaking error: %v", err)
	}
}
