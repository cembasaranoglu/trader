package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

type fakeREST struct {
	mu         sync.Mutex
	depthErr   error
	depthCalls int
	depthID    int64
	klineErr   error
	klineCalls int
	ranges     [][2]time.Time
	klines     []exchange.Kline
}

func (f *fakeREST) Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depthCalls++
	if f.depthErr != nil {
		return exchange.OrderBook{}, f.depthErr
	}
	return exchange.OrderBook{LastUpdateID: f.depthID,
		Bids: []exchange.Level{{Price: rat("100"), Qty: rat("1")}},
		Asks: []exchange.Level{{Price: rat("101"), Qty: rat("1")}}}, nil
}

func (f *fakeREST) KlinesRange(ctx context.Context, s exchange.Symbol, interval string, start, end time.Time, limit int) ([]exchange.Kline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.klineCalls++
	f.ranges = append(f.ranges, [2]time.Time{start, end})
	if f.klineErr != nil {
		return nil, f.klineErr
	}
	return f.klines, nil
}

type sink struct {
	mu      sync.Mutex
	bars    []Bar
	trades  []Trade
	tickers []Ticker
}

func (s *sink) OnBar(b Bar)       { s.mu.Lock(); s.bars = append(s.bars, b); s.mu.Unlock() }
func (s *sink) OnTrade(t Trade)   { s.mu.Lock(); s.trades = append(s.trades, t); s.mu.Unlock() }
func (s *sink) OnTicker(t Ticker) { s.mu.Lock(); s.tickers = append(s.tickers, t); s.mu.Unlock() }
func (s *sink) barCount() int     { s.mu.Lock(); defer s.mu.Unlock(); return len(s.bars) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func newManager(t *testing.T, rest *fakeREST) (*Manager, *sink, *clock, context.CancelFunc) {
	t.Helper()
	clk := &clock{t: t0}
	sk := &sink{}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := New(ctx, Config{
		Symbols:        []exchange.Symbol{{Name: "BTCUSDT", Filters: exchange.Filters{TickSize: rat("0.01"), StepSize: rat("0.00001")}}},
		Intervals:      []string{"1m"},
		DepthLimit:     1000,
		MaxDataAge:     5 * time.Second,
		GapFillRetries: 2,
		GapFillBackoff: 10 * time.Millisecond,
		Now:            clk.Now,
	}, rest, sk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	return m, sk, clk, cancel
}

func msg(ev any, at time.Time) ws.Message { return ws.Message{Event: ev, Received: at} }

func kline(open time.Time, closed bool) *ws.KlineEvent {
	return &ws.KlineEvent{Symbol: "BTCUSDT", Interval: "1m", OpenTime: open, CloseTime: open.Add(time.Minute - time.Millisecond),
		Open: "1", High: "2", Low: "0.5", Close: "1.5", Volume: "10", QuoteVolume: "15", Trades: 3, Closed: closed}
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamsNames(t *testing.T) {
	t.Parallel()
	got := Config{Symbols: []exchange.Symbol{{Name: "BTCUSDT"}}, Intervals: []string{"1m", "1M"}}.Streams()
	want := []string{"btcusdt@depth@100ms", "btcusdt@bookTicker", "btcusdt@aggTrade", "btcusdt@kline_1m", "btcusdt@kline_1M"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%v", got)
	}
}

func TestBookBecomesLiveAndFreshness(t *testing.T) {
	t.Parallel()
	rest := &fakeREST{depthID: 10}
	m, _, clk, _ := newManager(t, rest)
	if !m.Stale("BTCUSDT") {
		t.Fatal("no data yet: must be stale")
	}
	m.Handle(msg(&ws.DepthEvent{Symbol: "BTCUSDT", FirstUpdateID: 10, FinalUpdateID: 11, Bids: [][2]string{{"100.00", "2"}}}, clk.Now()))
	eventually(t, func() bool { v, _ := m.Book("btcusdt", 1); return v.State.String() == "live" })
	if v, _ := m.Book("BTCUSDT", 1); v.Bids[0].Qty.Cmp(rat("2")) != 0 || v.LastUpdateID != 11 {
		t.Fatalf("%+v", v)
	}
	if m.Stale("BTCUSDT") {
		t.Fatal("fresh live book reported stale")
	}
	clk.Advance(5*time.Second + time.Millisecond)
	if !m.Stale("BTCUSDT") {
		t.Fatal("R-9: data older than MaxDataAge must be stale")
	}
	if !m.Stale("ETHUSDT") {
		t.Fatal("unknown symbol must be stale")
	}
}

func TestSnapshotErrorAndRetry(t *testing.T) {
	t.Parallel()
	rest := &fakeREST{depthID: 10, depthErr: errors.New("boom")}
	m, _, clk, _ := newManager(t, rest)
	m.Handle(msg(&ws.DepthEvent{Symbol: "BTCUSDT", FirstUpdateID: 10, FinalUpdateID: 11}, clk.Now()))
	eventually(t, func() bool { return m.Stats()["BTCUSDT"].SnapshotErrs == 1 })
	rest.mu.Lock()
	rest.depthErr = nil
	rest.mu.Unlock()
	m.RetrySnapshots()
	eventually(t, func() bool { return m.Stats()["BTCUSDT"].BookState == "live" })
}

func TestDedupTradesAndTickers(t *testing.T) {
	t.Parallel()
	m, sk, clk, _ := newManager(t, &fakeREST{})
	for _, id := range []int64{5, 5, 4, 6, 9} {
		m.Handle(msg(&ws.AggTradeEvent{Symbol: "BTCUSDT", AggID: id, Price: "1", Qty: "1", TradeTime: t0}, clk.Now()))
	}
	for _, id := range []int64{7, 7, 8} {
		m.Handle(msg(&ws.BookTickerEvent{Symbol: "BTCUSDT", UpdateID: id, BidPrice: "1", BidQty: "1", AskPrice: "2", AskQty: "1"}, clk.Now()))
	}
	st := m.Stats()["BTCUSDT"]
	if len(sk.trades) != 3 || st.TradeDupes != 2 || st.TradeGaps != 2 {
		t.Fatalf("trades %d dupes %d gaps %d", len(sk.trades), st.TradeDupes, st.TradeGaps)
	}
	if len(sk.tickers) != 2 || st.TickerDupes != 1 {
		t.Fatalf("tickers %d dupes %d", len(sk.tickers), st.TickerDupes)
	}
	m.Handle(msg(&ws.AggTradeEvent{Symbol: "ETHUSDT", AggID: 1, Price: "1", Qty: "1"}, clk.Now()))
	if len(sk.trades) != 3 {
		t.Fatal("unknown symbol must be ignored")
	}
}

func TestKlinesOpenIgnoredAndDuplicatesDropped(t *testing.T) {
	t.Parallel()
	m, sk, clk, _ := newManager(t, &fakeREST{})
	m.Handle(msg(kline(t0, false), clk.Now()))
	m.Handle(msg(kline(t0, true), clk.Now()))
	m.Handle(msg(kline(t0, true), clk.Now())) // duplicate from overlapping connection
	m.Handle(msg(kline(t0.Add(time.Minute), true), clk.Now()))
	if sk.barCount() != 2 || sk.bars[0].Close.Cmp(rat("1.5")) != 0 || sk.bars[1].FromREST {
		t.Fatalf("bars %+v", sk.bars)
	}
}

func TestKlineGapFilledFromREST(t *testing.T) {
	t.Parallel()
	rest := &fakeREST{}
	for i := 1; i <= 2; i++ {
		o := t0.Add(time.Duration(i) * time.Minute)
		rest.klines = append(rest.klines, exchange.Kline{OpenTime: o, CloseTime: o.Add(time.Minute - time.Millisecond),
			Open: rat("1"), High: rat("1"), Low: rat("1"), Close: rat("1"), Volume: rat("1"), QuoteVol: rat("1")})
	}
	m, sk, clk, _ := newManager(t, rest)
	m.Handle(msg(kline(t0, true), clk.Now()))
	m.Handle(msg(kline(t0.Add(3*time.Minute), true), clk.Now())) // 10:01 and 10:02 missing
	m.Handle(msg(kline(t0.Add(4*time.Minute), true), clk.Now())) // arrives while filling
	eventually(t, func() bool { return sk.barCount() == 5 })
	sk.mu.Lock()
	defer sk.mu.Unlock()
	for i, b := range sk.bars {
		if !b.OpenTime.Equal(t0.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("bar %d out of order: %s", i, b.OpenTime)
		}
		if b.GapBefore {
			t.Fatalf("bar %d marked GapBefore after a complete fill", i)
		}
	}
	if !sk.bars[1].FromREST || !sk.bars[2].FromREST || sk.bars[3].FromREST {
		t.Fatal("FromREST flags wrong")
	}
	if r := rest.ranges[0]; !r[0].Equal(t0.Add(time.Minute)) || !r[1].Equal(t0.Add(3*time.Minute-time.Millisecond)) {
		t.Fatalf("range %v", r)
	}
	if m.Stats()["BTCUSDT"].BarGapsUnfill != 0 {
		t.Fatal("no unfilled gap expected")
	}
}

func TestKlineGapFillFailureMarksGap(t *testing.T) {
	t.Parallel()
	rest := &fakeREST{klineErr: errors.New("down")}
	m, sk, clk, _ := newManager(t, rest)
	m.Handle(msg(kline(t0, true), clk.Now()))
	m.Handle(msg(kline(t0.Add(3*time.Minute), true), clk.Now()))
	eventually(t, func() bool { return sk.barCount() == 2 })
	if !sk.bars[1].GapBefore || m.Stats()["BTCUSDT"].BarGapsUnfill != 1 {
		t.Fatalf("unrecovered gap must be flagged: %+v", sk.bars[1])
	}
	if rest.klineCalls != 2 {
		t.Fatalf("want 2 attempts, got %d", rest.klineCalls)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	ok := Config{Symbols: []exchange.Symbol{{Name: "X"}}, DepthLimit: 100, MaxDataAge: time.Second}
	for name, mut := range map[string]func(*Config){
		"no symbols":   func(c *Config) { c.Symbols = nil },
		"depth 0":      func(c *Config) { c.DepthLimit = 0 },
		"depth 5001":   func(c *Config) { c.DepthLimit = 5001 },
		"no max age":   func(c *Config) { c.MaxDataAge = 0 },
		"bad interval": func(c *Config) { c.Intervals = []string{"2m"} },
	} {
		c := ok
		mut(&c)
		if _, err := New(context.Background(), c, &fakeREST{}, &sink{}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNextOpen(t *testing.T) {
	t.Parallel()
	jan31 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, _ := NextOpen("1M", jan31); !n.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("1M: %s", n)
	}
	for iv, d := range map[string]time.Duration{"1s": time.Second, "15m": 15 * time.Minute, "4h": 4 * time.Hour, "1d": 24 * time.Hour, "3d": 72 * time.Hour, "1w": 168 * time.Hour} {
		if n, err := NextOpen(iv, t0); err != nil || n.Sub(t0) != d {
			t.Errorf("%s: %s %v", iv, n.Sub(t0), err)
		}
	}
	if _, err := NextOpen("2m", t0); !errors.Is(err, ErrUnknownInterval) {
		t.Fatal("unknown interval must fail")
	}
}

func TestStaleSnapshotRefetchIsDelayed(t *testing.T) {
	t.Parallel()
	rest := &fakeREST{depthID: 5} // always older than the stream
	m, _, clk, _ := newManager(t, rest)
	m.Handle(msg(&ws.DepthEvent{Symbol: "BTCUSDT", FirstUpdateID: 10, FinalUpdateID: 11}, clk.Now()))
	time.Sleep(700 * time.Millisecond)
	rest.mu.Lock()
	calls := rest.depthCalls
	rest.mu.Unlock()
	// Delays 0, 200ms, 400ms → at most 3 calls in 700ms (a tight loop makes thousands).
	if calls < 2 || calls > 4 {
		t.Fatalf("stale snapshots re-fetched %d times in 700ms", calls)
	}
}

// pagingREST returns contiguous 1m bars for any requested range, at most
// limit per call.
type pagingREST struct {
	fakeREST
	calls int
}

func (p *pagingREST) KlinesRange(ctx context.Context, s exchange.Symbol, interval string, start, end time.Time, limit int) ([]exchange.Kline, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	var out []exchange.Kline
	for o := start; !o.After(end) && len(out) < limit; o = o.Add(time.Minute) {
		out = append(out, exchange.Kline{OpenTime: o, CloseTime: o.Add(time.Minute - time.Millisecond),
			Open: rat("1"), High: rat("1"), Low: rat("1"), Close: rat("1"), Volume: rat("1"), QuoteVol: rat("1")})
	}
	return out, nil
}

func TestKlineGapFillPages(t *testing.T) {
	t.Parallel()
	rest := &pagingREST{}
	clk := &clock{t: t0}
	sk := &sink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel() }()
	m, err := New(ctx, Config{Symbols: []exchange.Symbol{{Name: "BTCUSDT"}}, Intervals: []string{"1m"}, DepthLimit: 100,
		MaxDataAge: time.Second, Now: clk.Now}, rest, sk)
	if err != nil {
		t.Fatal(err)
	}
	m.Handle(msg(kline(t0, true), clk.Now()))
	m.Handle(msg(kline(t0.Add(2501*time.Minute), true), clk.Now())) // 2500 bars missing → 3 pages
	eventually(t, func() bool { return sk.barCount() == 2502 })
	cancel()
	m.Wait()
	if rest.calls != 3 {
		t.Fatalf("want 3 pages, got %d", rest.calls)
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	for i, b := range sk.bars {
		if !b.OpenTime.Equal(t0.Add(time.Duration(i)*time.Minute)) || b.GapBefore {
			t.Fatalf("bar %d: %s gap=%v", i, b.OpenTime, b.GapBefore)
		}
	}
	if m.Stats()["BTCUSDT"].BarGapsUnfill != 0 {
		t.Fatal("complete paged fill must not count a gap")
	}
}
