package binance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/httpx"
)

func TestNewRejectsInsecureURLs(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]Options{
		"http base":         {BaseURL: "http://api.binance.com"},
		"http market":       {MarketDataURL: "http://data-api.binance.vision"},
		"query in base":     {BaseURL: "https://api.binance.com?x=1"},
		"recv window 0.5ms": {RecvWindow: 500 * time.Microsecond},
		"negative skew":     {MaxClockSkew: -time.Second},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Add(1)
	}))
	t.Cleanup(target.Close)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}), hmacCreds(t), nil)
	_, err := c.Account(context.Background())
	if !errors.Is(err, httpx.ErrRedirect) {
		t.Fatalf("want ErrRedirect, got %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("request with API key reached the redirect target")
	}
}

func TestCallerHTTPClientNotModified(t *testing.T) {
	t.Parallel()
	hc := &http.Client{}
	if _, err := New(Options{HTTPClient: hc}); err != nil {
		t.Fatal(err)
	}
	if hc.CheckRedirect != nil {
		t.Fatal("caller's http.Client was modified")
	}
}

func TestMalformedResponses(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	tests := []struct {
		name string
		body string
		call func(c *Client) error
	}{
		{"null serverTime body", `null`, func(c *Client) error { _, err := c.ServerTime(context.Background()); return err }},
		{"missing serverTime", `{}`, func(c *Client) error { _, err := c.ServerTime(context.Background()); return err }},
		{"null exchangeInfo", `null`, func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"no symbols", `{"symbols":[]}`, func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"symbol missing status", `{"symbols":[{"symbol":"X","baseAsset":"A","quoteAsset":"B","orderTypes":[],"ocoAllowed":true,"isSpotTradingAllowed":true,"filters":[]}]}`,
			func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"notional without apply flags", symbolWithFilters(`{"filterType":"NOTIONAL","minNotional":"5","maxNotional":"10"}`),
			func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"price filter missing tick", symbolWithFilters(`{"filterType":"PRICE_FILTER","minPrice":"0","maxPrice":"0"}`),
			func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"negative decimal", symbolWithFilters(`{"filterType":"LOT_SIZE","stepSize":"-1","minQty":"0","maxQty":"1"}`),
			func(c *Client) error { _, err := c.Symbols(context.Background()); return err }},
		{"null tickers", `null`, func(c *Client) error { _, err := c.Tickers24h(context.Background()); return err }},
		{"empty tickers", `[]`, func(c *Client) error { _, err := c.Tickers24h(context.Background()); return err }},
		{"ticker exponent", `[{"symbol":"X","lastPrice":"1e3","quoteVolume":"1"}]`, func(c *Client) error { _, err := c.Tickers24h(context.Background()); return err }},
		{"depth missing id", `{"bids":[],"asks":[]}`, func(c *Client) error { _, err := c.Depth(context.Background(), sym("X"), 5); return err }},
		{"depth missing asks", `{"lastUpdateId":1,"bids":[]}`, func(c *Client) error { _, err := c.Depth(context.Background(), sym("X"), 5); return err }},
		{"kline close before open", `[[2000,"1","1","1","1","1",1000,"1",1]]`, func(c *Client) error { _, err := c.Klines(context.Background(), sym("X"), "1m", 1); return err }},
		{"account missing flags", `{"commissionRates":{"maker":"0.001","taker":"0.001"},"balances":[]}`, func(c *Client) error { _, err := c.Account(context.Background()); return err }},
		{"account missing rates", `{"canTrade":true,"canWithdraw":false,"canDeposit":true,"balances":[]}`, func(c *Client) error { _, err := c.Account(context.Background()); return err }},
		{"account missing balances", `{"canTrade":true,"canWithdraw":false,"canDeposit":true,"commissionRates":{"maker":"0.001","taker":"0.001"}}`,
			func(c *Client) error { _, err := c.Account(context.Background()); return err }},
		{"permissions missing withdrawals", `{"ipRestrict":true,"enableReading":true,"enableSpotAndMarginTrading":true}`,
			func(c *Client) error { _, err := c.KeyPermissions(context.Background()); return err }},
		{"permissions non-bool", `{"ipRestrict":true,"enableReading":true,"enableSpotAndMarginTrading":true,"enableWithdrawals":"false"}`,
			func(c *Client) error { _, err := c.KeyPermissions(context.Background()); return err }},
		{"permissions null body", `null`, func(c *Client) error { _, err := c.KeyPermissions(context.Background()); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}), hmacCreds(t), clk)
			if err := tc.call(c); !errors.Is(err, ErrBadResponse) {
				t.Fatalf("want ErrBadResponse, got %v", err)
			}
		})
	}
}

func symbolWithFilters(filters string) string {
	return `{"symbols":[{"symbol":"X","status":"TRADING","baseAsset":"A","quoteAsset":"B","orderTypes":["LIMIT"],` +
		`"ocoAllowed":true,"isSpotTradingAllowed":true,"filters":[` + filters + `]}]}`
}

func TestFilterSemantics(t *testing.T) {
	t.Parallel()
	notional := `{"filterType":"NOTIONAL","minNotional":"5","applyMinToMarket":false,"maxNotional":"100","applyMaxToMarket":true}`
	minNotional := `{"filterType":"MIN_NOTIONAL","minNotional":"10","applyToMarket":true}`
	for name, filters := range map[string]string{
		"notional first": notional + "," + minNotional,
		"notional last":  minNotional + "," + notional,
	} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, symbolWithFilters(filters+`,{"filterType":"PRICE_FILTER","minPrice":"0","maxPrice":"0","tickSize":"0.01"}`))
		}), nil, nil)
		syms, err := c.Symbols(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f := syms[0].Filters
		if !eqRat(f.MinNotional, rat(t, "5")) || f.NotionalAppliesToMarket || !eqRat(f.MaxNotional, rat(t, "100")) || !f.MaxNotionalAppliesToMarket {
			t.Fatalf("%s: NOTIONAL must take precedence: %+v", name, f)
		}
		if f.MinPrice != nil || f.MaxPrice != nil || !eqRat(f.TickSize, rat(t, "0.01")) {
			t.Fatalf("%s: zero PRICE_FILTER bounds must be nil (disabled): %+v", name, f)
		}
	}
}

func TestKeyPermissionsListsUnknownTrueFlags(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ipRestrict":true,"enableReading":true,"enableSpotAndMarginTrading":true,"enableWithdrawals":false,`+
			`"createTime":1,"newShinyPermission":true,"tradingAuthorityExpirationTime":5}`)
	}), hmacCreds(t), nil)
	p, err := c.KeyPermissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !p.IPRestricted || !p.SpotTrading || p.Withdrawals || strings.Join(p.OtherEnabled, ",") != "newShinyPermission" {
		t.Fatalf("%+v", p)
	}
}

func TestOversizedResponse(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat(" ", 1<<20)
		for i := 0; i <= MaxBodyBytes>>20; i++ {
			fmt.Fprint(w, chunk)
		}
	}), nil, nil)
	if _, err := c.ServerTime(context.Background()); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ServerTime(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestSyncTimeAppliesOffset(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	var gotTimestamp atomic.Int64
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/base/api/v3/time":
			fmt.Fprint(w, `{"serverTime":1700000000750}`) // server is 750ms ahead
		default:
			raw := r.URL.RawQuery
			q, _ := url.ParseQuery(raw[:strings.LastIndex(raw, "&signature=")])
			var ts int64
			fmt.Sscan(q.Get("timestamp"), &ts)
			gotTimestamp.Store(ts)
			w.Write(fixture(t, "api_restrictions.json"))
		}
	}), hmacCreds(t), clk)
	off, err := c.SyncTime(context.Background())
	if err != nil || off != 750*time.Millisecond || c.ClockOffset() != off {
		t.Fatalf("offset %s err %v", off, err)
	}
	if _, err := c.KeyPermissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotTimestamp.Load() != 1700000000750 {
		t.Fatalf("timestamp %d does not include the offset", gotTimestamp.Load())
	}
}

func TestSyncTimeRefusesLargeSkew(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"serverTime":1700000002000}`)
	}))
	t.Cleanup(srv.Close)
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	c, err := New(Options{BaseURL: srv.URL, MarketDataURL: srv.URL, HTTPClient: srv.Client(), Now: clk.Now, MaxClockSkew: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SyncTime(context.Background()); !errors.Is(err, ErrClockSkew) {
		t.Fatalf("want ErrClockSkew, got %v", err)
	}
	if c.ClockOffset() != 0 {
		t.Fatal("rejected offset must not be stored")
	}
}

func TestRetryAfterClampedAndBlockedUntil(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "999999999999")
		w.WriteHeader(http.StatusTooManyRequests)
	}), nil, clk)
	_, err := c.ServerTime(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != httpx.MaxRetryAfter {
		t.Fatalf("got %v", err)
	}
	if want := clk.Now().Add(httpx.MaxRetryAfter); !c.BlockedUntil().Equal(want) {
		t.Fatalf("BlockedUntil %s, want %s", c.BlockedUntil(), want)
	}
}

func TestSignerValuesAreRedacted(t *testing.T) {
	t.Parallel()
	s, _ := NewHMACSigner("test-secret")
	pemBytes, _ := ed25519PEM(t)
	ed, _ := NewEd25519SignerFromPEM(pemBytes)
	out := fmt.Sprintf("%v %+v %#v %s %v %+v %#v", *s, *s, *s, *s, *ed, *ed, *ed)
	if strings.Contains(out, "test-secret") || strings.Contains(out, "116 101 115 116") || strings.Contains(out, "key:") {
		t.Fatalf("leak: %s", out)
	}
}

func TestKlinesRejectsUnknownInterval(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), nil, nil)
	if _, err := c.Klines(context.Background(), sym("X"), "2m", 1); err == nil || hits.Load() != 0 {
		t.Fatalf("unknown interval must fail locally: %v", err)
	}
}

func sym(name string) exchange.Symbol { return exchange.Symbol{Name: name} }
