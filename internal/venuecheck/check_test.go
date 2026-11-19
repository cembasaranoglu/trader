package venuecheck

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binancetr"
)

type fakeVenue struct {
	now        time.Time
	serverSkew time.Duration
	symbols    []exchange.Symbol
	symErr     error
	book       exchange.OrderBook
	depthErr   error
	klines     []exchange.Kline
	account    exchange.AccountInfo
	accErr     error
	accCalls   int
}

func (f *fakeVenue) ServerTime(context.Context) (time.Time, error) {
	return f.now.Add(f.serverSkew), nil
}
func (f *fakeVenue) Symbols(context.Context) ([]exchange.Symbol, error) {
	return f.symbols, f.symErr
}
func (f *fakeVenue) Depth(context.Context, exchange.Symbol, int) (exchange.OrderBook, binancetr.Shape, error) {
	return f.book, binancetr.ShapeRaw, f.depthErr
}
func (f *fakeVenue) Klines(context.Context, exchange.Symbol, string, int) ([]exchange.Kline, binancetr.Shape, error) {
	return f.klines, binancetr.ShapeEnvelope, nil
}
func (f *fakeVenue) Account(context.Context) (exchange.AccountInfo, error) {
	f.accCalls++
	return f.account, f.accErr
}
func (f *fakeVenue) Usage() map[string]string { return map[string]string{"x-mbx-used-weight-1m": "7"} }

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
			{Name: "BTC_TRY", Quote: "TRY", Type: 1, SpotEnabled: true, OCOEnabled: true, STPDefault: "EXPIRE_MAKER",
				Filters: exchange.Filters{TickSize: r("1"), StepSize: r("0.00001"), MinQty: r("0.00001"), MinNotional: r("100"), NotionalAppliesToMarket: true}},
			{Name: "NEW_TRY", Quote: "TRY", Type: 2},
			{Name: "ADA_BNB", Quote: "BNB", Type: 1, SpotEnabled: true},
		},
		book: exchange.OrderBook{Bids: []exchange.Level{{Price: r("99"), Qty: r("1")}}, Asks: []exchange.Level{{Price: r("101"), Qty: r("1")}}},
		// Oldest first: one closed candle, then the still-open current candle.
		klines:  []exchange.Kline{{CloseTime: now.Add(-30 * time.Second)}, {CloseTime: now.Add(30 * time.Second)}},
		account: exchange.AccountInfo{CanTrade: true, FiatTakerRate: r("0.0015"), Balances: []exchange.Balance{{Asset: "TRY", Free: r("1234.5")}}},
	}
}

func TestRunReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	fv := newFake(now)
	rep, err := Run(context.Background(), fv, Options{
		Symbols: []string{"BTC_TRY", "XYZ_TRY"}, ClockSamples: 3, CheckAccount: true, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clock == nil || rep.Clock.Samples != 3 || rep.Clock.MedianOffsetMs != 120 {
		t.Fatalf("clock %+v", rep.Clock)
	}
	if s := rep.Symbols; s.Total != 3 || s.TRYQuoted != 2 || s.TRYSpotEnabled != 1 || s.TRYByType[1] != 1 || s.TRYByType[2] != 1 || s.NonTRYQuoted != 1 {
		t.Fatalf("symbols %+v", s)
	}
	if len(rep.Watched) != 2 || rep.Watched[1].Found {
		t.Fatalf("watched %+v", rep.Watched)
	}
	w := rep.Watched[0]
	if !w.Found || w.MinNotional != "100" || w.StepSize != "0.00001" || w.DepthShape != "raw" || w.KlinesShape != "envelope" {
		t.Fatalf("watched[0] %+v", w)
	}
	if w.SpreadBps == nil || *w.SpreadBps != 200 {
		t.Fatalf("spread %v", w.SpreadBps)
	}
	if w.LastKlineCloseAgeSec == nil || *w.LastKlineCloseAgeSec != 30 {
		t.Fatalf("kline age %v", w.LastKlineCloseAgeSec)
	}
	if rep.Account == nil || rep.Account.AssetCount != 1 || rep.Account.FiatTakerRate != "0.0015" {
		t.Fatalf("account %+v", rep.Account)
	}
	if rep.RateLimitHeaders["x-mbx-used-weight-1m"] != "7" || len(rep.Errors) != 0 || len(rep.ManualChecks) == 0 {
		t.Fatalf("report %+v", rep)
	}

	// The shareable report must not contain balance amounts.
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "1234.5") {
		t.Fatal("report leaks balance amounts")
	}
}

func TestRunCollectsErrorsAndSkipsAccount(t *testing.T) {
	t.Parallel()
	now := time.Now()
	fv := newFake(now)
	fv.depthErr = errors.New("boom")
	rep, err := Run(context.Background(), fv, Options{Symbols: []string{"BTC_TRY"}, CheckAccount: false, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if fv.accCalls != 0 || rep.AccountSkipped == "" || rep.Account != nil {
		t.Fatalf("account must be skipped: %+v", rep)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "depth BTC_TRY") {
		t.Fatalf("errors %v", rep.Errors)
	}
}

func TestRunStopsOnContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, newFake(time.Now()), Options{ClockSamples: 2, Pause: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSpreadBpsEdgeCases(t *testing.T) {
	t.Parallel()
	if spreadBps(r("101"), r("99")) != nil {
		t.Error("crossed book must yield nil")
	}
	if spreadBps(r("0"), r("1")) != nil {
		t.Error("zero bid must yield nil")
	}
}

func TestMedian(t *testing.T) {
	t.Parallel()
	if median([]int64{5, 1, 3}) != 3 || median([]int64{4, 1, 3, 2}) != 2 {
		t.Fatal("median")
	}
}

func TestLoadCredentials(t *testing.T) {
	t.Parallel()
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
	ok := write("ok.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o600)
	c, err := LoadCredentials(ok)
	if err != nil || c.APIKey != "k" || c.SecretKey != "s3cr3t" {
		t.Fatalf("valid file: %v", err)
	}
	tests := []struct {
		name string
		path string
		want error
	}{
		{"group readable", write("open.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o640), ErrInsecureCredentialsFile},
		{"missing secret", write("half.json", `{"api_key":"k"}`, 0o600), nil},
		{"unknown field", write("extra.json", `{"api_key":"k","secret_key":"s3cr3t","x":1}`, 0o600), nil},
		{"not json", write("bad.json", `api_key=s3cr3t`, 0o600), nil},
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
