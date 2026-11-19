package venuecheck

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

type fakeVenue struct {
	now        time.Time
	serverSkew time.Duration
	symbols    []exchange.Symbol
	book       exchange.OrderBook
	depthErr   error
	klines     []exchange.Kline
	account    exchange.AccountInfo
	accCalls   int
}

func (f *fakeVenue) ServerTime(context.Context) (time.Time, error) {
	return f.now.Add(f.serverSkew), nil
}
func (f *fakeVenue) Symbols(context.Context) ([]exchange.Symbol, error) {
	return f.symbols, nil
}
func (f *fakeVenue) Depth(context.Context, exchange.Symbol, int) (exchange.OrderBook, error) {
	return f.book, f.depthErr
}
func (f *fakeVenue) Klines(context.Context, exchange.Symbol, string, int) ([]exchange.Kline, error) {
	return f.klines, nil
}
func (f *fakeVenue) Account(context.Context) (exchange.AccountInfo, error) {
	f.accCalls++
	return f.account, nil
}
func (f *fakeVenue) Usage() map[string]string { return map[string]string{"x-mbx-used-weight-1m": "7"} }

// fullVenue adds the optional ticker and permission sources.
type fullVenue struct {
	*fakeVenue
	tickers []exchange.Ticker
	perms   exchange.KeyPermissions
}

func (f fullVenue) Tickers24h(context.Context) ([]exchange.Ticker, error) { return f.tickers, nil }
func (f fullVenue) KeyPermissions(context.Context) (exchange.KeyPermissions, error) {
	return f.perms, nil
}

func r(s string) *big.Rat {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return v
}

func newFake(now time.Time) *fakeVenue {
	return &fakeVenue{
		now:        now,
		serverSkew: 120 * time.Millisecond,
		symbols: []exchange.Symbol{
			{Name: "BTCUSDT", Quote: "USDT", Status: "TRADING", SpotEnabled: true, OCOEnabled: true,
				Filters: exchange.Filters{TickSize: r("0.01"), StepSize: r("0.00001"), MinQty: r("0.00001"), MinNotional: r("5"), NotionalAppliesToMarket: true}},
			{Name: "ETHUSDT", Quote: "USDT", Status: "TRADING", SpotEnabled: true},
			{Name: "OLDUSDT", Quote: "USDT", Status: "BREAK"},
			{Name: "ETHBTC", Quote: "BTC", Status: "TRADING", SpotEnabled: true},
		},
		book: exchange.OrderBook{Bids: []exchange.Level{{Price: r("99"), Qty: r("1")}}, Asks: []exchange.Level{{Price: r("101"), Qty: r("1")}}},
		// Oldest first: one closed candle, then the still-open current candle.
		klines:  []exchange.Kline{{CloseTime: now.Add(-30 * time.Second)}, {CloseTime: now.Add(30 * time.Second)}},
		account: exchange.AccountInfo{CanTrade: true, TakerRate: r("0.001"), Balances: []exchange.Balance{{Asset: "USDT", Free: r("1234.5")}}},
	}
}

func TestRunFullReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	fv := fullVenue{
		fakeVenue: newFake(now),
		tickers: []exchange.Ticker{
			{Symbol: "ETHUSDT", LastPrice: r("3000"), QuoteVolume: r("500")},
			{Symbol: "BTCUSDT", LastPrice: r("90000.5"), QuoteVolume: r("900.4")},
			{Symbol: "OLDUSDT", LastPrice: r("1"), QuoteVolume: r("99999")},   // not tradable
			{Symbol: "ETHBTC", LastPrice: r("0.03"), QuoteVolume: r("99999")}, // other quote
		},
		perms: exchange.KeyPermissions{Reading: true, Withdrawals: true, SpotTrading: true, OtherEnabled: []string{"enableMargin"}},
	}
	rep, err := Run(context.Background(), fv, Options{
		Venue: "binance", Quote: "USDT", Symbols: []string{"BTCUSDT", "XYZUSDT"}, TopN: 5,
		ClockSamples: 3, CheckAccount: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clock == nil || rep.Clock.MedianOffsetMs != 120 {
		t.Fatalf("clock %+v", rep.Clock)
	}
	if s := rep.Symbols; s.Total != 4 || s.QuoteCount != 3 || s.QuoteSpotEnabled != 2 || s.ByType != nil {
		t.Fatalf("symbols %+v", s)
	}
	if len(rep.TopByQuoteVolume) != 2 || rep.TopByQuoteVolume[0].Symbol != "BTCUSDT" || rep.TopByQuoteVolume[0].QuoteVolume24h != "900" {
		t.Fatalf("top %+v", rep.TopByQuoteVolume)
	}
	w := rep.Watched[0]
	if !w.Found || w.MinNotional != "5" || w.TickSize != "0.01" || w.SpreadBps == nil || *w.SpreadBps != 200 ||
		w.LastClosedKlineAgeSec == nil || *w.LastClosedKlineAgeSec != 30 || rep.Watched[1].Found {
		t.Fatalf("watched %+v", rep.Watched)
	}
	if rep.Account == nil || rep.Account.TakerRate != "0.001" || rep.Account.AssetCount != 1 {
		t.Fatalf("account %+v", rep.Account)
	}
	if rep.KeyPermissions == nil || !rep.KeyPermissions.Withdrawals {
		t.Fatalf("perms %+v", rep.KeyPermissions)
	}
	joined := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{"WITHDRAWALS", "without an IP restriction", "enableMargin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q: %v", want, rep.Warnings)
		}
	}
	for _, mc := range rep.ManualChecks {
		if strings.Contains(mc, "IP whitelist") {
			t.Error("manual IP check must be dropped when the venue exposes permissions")
		}
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "1234.5") {
		t.Fatal("report leaks balance amounts")
	}
}

func TestRunBasicVenueSkipsAccount(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fv := newFake(now)
	fv.depthErr = errors.New("boom")
	rep, err := Run(context.Background(), fv, Options{Quote: "USDT", Symbols: []string{"BTCUSDT"}, TopN: 5, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if fv.accCalls != 0 || rep.AccountSkipped == "" || rep.TopByQuoteVolume != nil || rep.KeyPermissions != nil {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "depth BTCUSDT") {
		t.Fatalf("errors %v", rep.Errors)
	}
	if len(rep.ManualChecks) != 3 {
		t.Fatalf("basic venue needs manual permission checks: %v", rep.ManualChecks)
	}
}

func TestRunStopsOnContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, newFake(time.Now()), Options{ClockSamples: 2, Pause: time.Hour}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestHelpers(t *testing.T) {
	t.Parallel()
	if spreadBps(r("101"), r("99")) != nil || spreadBps(r("0"), r("1")) != nil {
		t.Error("crossed or zero book must yield nil")
	}
	if median([]int64{5, 1, 3}) != 3 || median([]int64{4, 1, 3, 2}) != 2 {
		t.Error("median")
	}
	if dec(r("0.00100000")) != "0.001" || dec(r("10")) != "10" || dec(nil) != "" {
		t.Error("dec")
	}
}

func TestLoadCredentials(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows; see private_other.go")
	}
	dir := t.TempDir()
	write := func(name, body string, perm os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil { // WriteFile honors umask
			t.Fatal(err)
		}
		return p
	}
	hmac := write("hmac.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o600)
	c, err := LoadCredentials(hmac)
	if err != nil || c.SecretKey != "s3cr3t" || c.Ed25519PEM != nil {
		t.Fatalf("hmac: %v", err)
	}
	pemPath := write("key.pem", "-----BEGIN PRIVATE KEY-----\ns3cr3t\n-----END PRIVATE KEY-----\n", 0o600)
	ed := write("ed.json", `{"api_key":"k","ed25519_private_key_file":"`+pemPath+`"}`, 0o600)
	c, err = LoadCredentials(ed)
	if err != nil || c.SecretKey != "" || !strings.Contains(string(c.Ed25519PEM), "PRIVATE KEY") {
		t.Fatalf("ed25519: %v", err)
	}
	if s := strings.Join([]string{c.String(), c.GoString()}, " "); strings.Contains(s, "s3cr3t") {
		t.Fatal("credentials printed")
	}
	rel := write("rel.json", `{"api_key":"k","ed25519_private_key_file":"key.pem"}`, 0o600)
	if c, err = LoadCredentials(rel); err != nil || c.Ed25519PEM == nil {
		t.Fatalf("relative key path must resolve against the credentials directory: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(hmac, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(link); err == nil {
		t.Fatal("symlinked credentials file must be rejected")
	}

	openPEM := write("open.pem", "x", 0o644)
	tests := []struct {
		name string
		path string
		want error
	}{
		{"group readable", write("open.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o640), ErrInsecureCredentialsFile},
		{"key file group readable", write("edopen.json", `{"api_key":"k","ed25519_private_key_file":"`+openPEM+`"}`, 0o600), ErrInsecureCredentialsFile},
		{"both secrets", write("both.json", `{"api_key":"k","secret_key":"s3cr3t","ed25519_private_key_file":"x"}`, 0o600), nil},
		{"no secret", write("half.json", `{"api_key":"k"}`, 0o600), nil},
		{"no api key", write("nokey.json", `{"secret_key":"s3cr3t"}`, 0o600), nil},
		{"unknown field", write("extra.json", `{"api_key":"k","secret_key":"s3cr3t","x":1}`, 0o600), nil},
		{"not json", write("bad.json", `api_key=s3cr3t`, 0o600), nil},
		{"trailing data", write("trail.json", `{"api_key":"k","secret_key":"s3cr3t"} {"x":"s3cr3t"}`, 0o600), nil},
		{"directory", dir, nil},
		{"missing file", filepath.Join(dir, "none.json"), os.ErrNotExist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadCredentials(tc.path)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error leaks file contents: %v", err)
			}
		})
	}
}
