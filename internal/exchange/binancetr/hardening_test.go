package binancetr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange/httpx"
)

func TestDoDoesNotMutateCallerQuery(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}), &Credentials{APIKey: "k", SecretKey: "s"}, nil)
	q := url.Values{"symbol": {"BTC_TRY"}}
	if _, err := c.do(context.Background(), request{method: http.MethodGet, host: c.base, path: "/x", query: q, signed: true}); err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || q.Get("timestamp") != "" || q.Get("recvWindow") != "" {
		t.Fatalf("caller query mutated: %v", q)
	}
}

func TestRedirectAndSchemeRules(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	t.Cleanup(target.Close)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}), &Credentials{APIKey: "k", SecretKey: "s"}, nil)
	if _, err := c.Account(context.Background()); !errors.Is(err, httpx.ErrRedirect) || leaked.Load() != 0 {
		t.Fatalf("redirect must be refused: err=%v leaked=%d", err, leaked.Load())
	}
	for _, o := range []Options{{BaseURL: "http://www.binance.tr"}, {MainMarketURL: "http://api.binance.me"}, {RecvWindow: time.Microsecond}} {
		if _, err := New(o); err == nil {
			t.Errorf("%+v must be rejected", o)
		}
	}
}

func TestNullBodyAndNullData(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"null body": `null`, "null data": `{"code":0,"data":null}`} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		}), nil, nil)
		if _, err := c.Symbols(context.Background()); err == nil {
			t.Errorf("%s: Symbols must fail", name)
		}
	}
}

func TestNotionalMarketFlag(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		filters string
		min     string
		market  bool
	}{
		"tr form applyToMarket": {`{"filterType":"NOTIONAL","minNotional":"100","applyToMarket":true}`, "100", true},
		"global form wins":      {`{"filterType":"NOTIONAL","minNotional":"100","applyToMarket":true,"applyMinToMarket":false}`, "100", false},
		"notional after min":    {`{"filterType":"MIN_NOTIONAL","minNotional":"50","applyToMarket":false},{"filterType":"NOTIONAL","minNotional":"100","applyMinToMarket":true}`, "100", true},
		"notional before min":   {`{"filterType":"NOTIONAL","minNotional":"100","applyMinToMarket":true},{"filterType":"MIN_NOTIONAL","minNotional":"50","applyToMarket":false}`, "100", true},
		"min notional only":     {`{"filterType":"MIN_NOTIONAL","minNotional":"50","applyToMarket":false}`, "50", false},
	}
	for name, tc := range tests {
		f, err := parseFilters(mustFilters(t, "["+tc.filters+"]"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.MinNotional == nil || f.MinNotional.RatString() != tc.min || f.NotionalAppliesToMarket != tc.market {
			t.Errorf("%s: got min=%v market=%v", name, f.MinNotional, f.NotionalAppliesToMarket)
		}
	}
}

func mustFilters(t *testing.T, raw string) []wireFilter {
	t.Helper()
	var ws []wireFilter
	if err := json.Unmarshal([]byte(raw), &ws); err != nil {
		t.Fatal(err)
	}
	return ws
}
