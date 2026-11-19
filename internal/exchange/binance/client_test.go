package binance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
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

func (f *fakeClock) Now() time.Time          { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) Advance(d time.Duration) { f.mu.Lock(); defer f.mu.Unlock(); f.t = f.t.Add(d) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestClient(t *testing.T, h http.Handler, creds *Credentials, clk *fakeClock) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts := Options{BaseURL: srv.URL + "/base", MarketDataURL: srv.URL + "/md", HTTPClient: srv.Client(), Credentials: creds}
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

func hmacCreds(t *testing.T) *Credentials {
	t.Helper()
	s, err := NewHMACSigner("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	return &Credentials{APIKey: "test-key", Signer: s}
}

func TestHMACKnownVector(t *testing.T) {
	t.Parallel()
	s, _ := NewHMACSigner("NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j")
	got, _ := s.Sign("symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559")
	if want := "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"; got != want {
		t.Fatalf("got %s", got)
	}
	if _, err := NewHMACSigner(""); err == nil {
		t.Fatal("empty secret must be rejected")
	}
}

func ed25519PEM(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), pub
}

func TestEd25519Signer(t *testing.T) {
	t.Parallel()
	pemBytes, pub := ed25519PEM(t)
	s, err := NewEd25519SignerFromPEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := s.Sign("a=1&b=2")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, []byte("a=1&b=2"), raw) {
		t.Fatal("signature does not verify")
	}
	for name, in := range map[string][]byte{
		"no pem":     []byte("hello"),
		"encrypted":  pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1}}),
		"wrong type": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}}),
		"garbage":    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
	} {
		if _, err := NewEd25519SignerFromPEM(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]Options{
		"bad url":       {BaseURL: "::"},
		"key no signer": {Credentials: &Credentials{APIKey: "k"}},
		"recv window":   {RecvWindow: time.Minute + time.Second},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := New(Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestServerTime(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/api/v3/time" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"serverTime":1700000000123}`)
	}), nil, nil)
	got, err := c.ServerTime(context.Background())
	if err != nil || got.UnixMilli() != 1700000000123 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestSymbols(t *testing.T) {
	t.Parallel()
	body := fixture(t, "exchange_info.json")
	var gotQuery url.Values
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Write(body)
	}), nil, nil)
	syms, err := c.Symbols(context.Background(), "BTCUSDT", "OLDUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("symbols") != `["BTCUSDT","OLDUSDT"]` {
		t.Errorf("symbols param %q", gotQuery.Get("symbols"))
	}
	if len(syms) != 3 {
		t.Fatalf("len %d", len(syms))
	}
	btc, old := syms[0], syms[1]
	if !btc.SpotEnabled || !btc.OCOEnabled || btc.Quote != "USDT" || !eqRat(btc.Filters.MinNotional, rat(t, "5")) ||
		!btc.Filters.NotionalAppliesToMarket || !eqRat(btc.Filters.TickSize, rat(t, "0.01")) {
		t.Fatalf("btc %+v", btc)
	}
	if old.SpotEnabled || old.Status != "BREAK" || !eqRat(old.Filters.MinNotional, rat(t, "10")) || !old.Filters.NotionalAppliesToMarket {
		t.Fatalf("old %+v", old)
	}
}

func TestTickers24h(t *testing.T) {
	t.Parallel()
	body := fixture(t, "ticker_mini.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "MINI" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		w.Write(body)
	}), nil, nil)
	ts, err := c.Tickers24h(context.Background())
	if err != nil || len(ts) != 2 || !eqRat(ts[0].QuoteVolume, rat(t, "90550000.25")) {
		t.Fatalf("%v %+v", err, ts)
	}
}

func TestDepthAndKlines(t *testing.T) {
	t.Parallel()
	depth, klines := fixture(t, "depth.json"), fixture(t, "klines.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "BTCUSDT" {
			t.Errorf("symbol %q", r.URL.Query().Get("symbol"))
		}
		switch r.URL.Path {
		case "/md/api/v3/depth":
			w.Write(depth)
		case "/md/api/v3/klines":
			w.Write(klines)
		default:
			t.Errorf("path %s", r.URL.Path)
		}
	}), nil, nil)
	s := exchange.Symbol{Name: "BTCUSDT"}
	ob, err := c.Depth(context.Background(), s, 5)
	if err != nil || !eqRat(ob.Asks[0].Price, rat(t, "90500.01")) {
		t.Fatalf("%v %+v", err, ob)
	}
	ks, err := c.Klines(context.Background(), s, "1m", 1)
	if err != nil || len(ks) != 1 || ks[0].Trades != 308 {
		t.Fatalf("%v %+v", err, ks)
	}
	if _, err := c.Depth(context.Background(), s, 0); err == nil {
		t.Error("depth limit 0 must fail")
	}
	if _, err := c.Klines(context.Background(), s, "", 1); err == nil {
		t.Error("empty interval must fail")
	}
}

// verifyHMAC checks the signed query of a request against the test secret.
func verifyHMAC(t *testing.T, r *http.Request, clk *fakeClock) {
	t.Helper()
	if r.Header.Get("X-MBX-APIKEY") != "test-key" {
		t.Error("missing api key header")
	}
	raw := r.URL.RawQuery
	i := strings.LastIndex(raw, "&signature=")
	if i < 0 {
		t.Fatalf("no signature in %q", raw)
	}
	payload, sig := raw[:i], raw[i+len("&signature="):]
	want, _ := (&HMACSigner{secret: []byte("test-secret")}).Sign(payload)
	if sig != want {
		t.Errorf("bad signature")
	}
	q, _ := url.ParseQuery(payload)
	if q.Get("timestamp") != fmt.Sprint(clk.Now().UnixMilli()) || q.Get("recvWindow") != "5000" {
		t.Errorf("timestamp/recvWindow %v", q)
	}
}

func TestAccount(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	body := fixture(t, "account.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/api/v3/account" || !strings.Contains(r.URL.RawQuery, "omitZeroBalances=true") {
			t.Errorf("request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		verifyHMAC(t, r, clk)
		w.Write(body)
	}), hmacCreds(t), clk)
	info, err := c.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.CanTrade || !eqRat(info.TakerRate, rat(t, "0.001")) || info.RawMakerCommission != "10" || len(info.Balances) != 2 {
		t.Fatalf("%+v", info)
	}
}

func TestKeyPermissions(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.UnixMilli(1700000000000)}
	body := fixture(t, "api_restrictions.json")
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/sapi/v1/account/apiRestrictions" {
			t.Errorf("path %s", r.URL.Path)
		}
		verifyHMAC(t, r, clk)
		w.Write(body)
	}), hmacCreds(t), clk)
	p, err := c.KeyPermissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.IPRestricted || !p.Reading || p.SpotTrading || p.Withdrawals {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.OtherEnabled, ",") != "enableFixReadOnly,enablePortfolioMarginTrading" {
		t.Fatalf("other %v", p.OtherEnabled)
	}
}

func TestEd25519SignedRequestVerifies(t *testing.T) {
	t.Parallel()
	pemBytes, pub := ed25519PEM(t)
	s, err := NewEd25519SignerFromPEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		i := strings.LastIndex(raw, "&signature=")
		sigEsc := raw[i+len("&signature="):]
		sigB64, err := url.QueryUnescape(sigEsc)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := base64.StdEncoding.DecodeString(sigB64)
		if err != nil || !ed25519.Verify(pub, []byte(raw[:i]), sig) {
			t.Error("ed25519 signature does not verify over the query payload")
		}
		w.Write(fixture(t, "api_restrictions.json"))
	}), &Credentials{APIKey: "k", Signer: s}, nil)
	if _, err := c.KeyPermissions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSignedWithoutCredentials(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), nil, nil)
	if _, err := c.Account(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatal(err)
	}
	if _, err := c.KeyPermissions(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatal("no request expected")
	}
}

func TestRateLimitBackoff(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	var hits atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTeapot)
			return
		}
		fmt.Fprint(w, `{"serverTime":1}`)
	}), nil, clk)
	_, err := c.ServerTime(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.ServerTime(context.Background()); !errors.Is(err, ErrBackoff) || hits.Load() != 1 {
		t.Fatalf("want local backoff, got %v hits=%d", err, hits.Load())
	}
	clk.Advance(7 * time.Second)
	if _, err := c.ServerTime(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status   int
		body     string
		sentinel error
		code     int
	}{
		{502, "", ErrServer, 0},
		{400, `{"code":-1121,"msg":"Invalid symbol."}`, ErrAPI, -1121},
		{403, "<html>waf</html>", ErrAPI, 0},
	} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}), nil, nil)
		_, err := c.ServerTime(context.Background())
		if !errors.Is(err, tc.sentinel) {
			t.Fatalf("%d: got %v", tc.status, err)
		}
		var api *APIError
		if errors.As(err, &api) && api.Code != tc.code {
			t.Fatalf("code %d", api.Code)
		}
	}
}

func TestUsageAndSecretsNeverPrinted(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "21")
		w.Header().Set("X-SAPI-USED-IP-WEIGHT-1M", "1")
		fmt.Fprint(w, `{"serverTime":1}`)
	}), nil, nil)
	if _, err := c.ServerTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := c.SignedHost()
	if u := c.Usage(); u[h+" x-mbx-used-weight-1m"] != "21" || u[h+" x-sapi-used-ip-weight-1m"] != "1" {
		t.Fatalf("usage %v", u)
	}

	creds := hmacCreds(t)
	pemBytes, _ := ed25519PEM(t)
	ed, _ := NewEd25519SignerFromPEM(pemBytes)
	for _, s := range []string{fmt.Sprintf("%v %+v %#v", *creds, *creds, *creds), fmt.Sprintf("%v %#v", creds.Signer, ed)} {
		if strings.Contains(s, "test-secret") || strings.Contains(s, "test-key") {
			t.Fatalf("leak: %s", s)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c2, _ := New(Options{BaseURL: srv.URL, Credentials: creds})
	if _, err := c2.Account(context.Background()); err == nil || strings.Contains(err.Error(), "signature=") {
		t.Fatalf("error leaks query: %v", err)
	}
}
