// Package marketdata turns the Binance combined market-data stream into
// validated, de-duplicated data for the rest of the system: closed klines
// (with REST gap filling), aggregate trades, best bid/ask, local order books
// and per-symbol freshness (R-9).
//
// One Manager owns all state behind a mutex. Stream messages arrive from a
// single goroutine (ws.Client.Run); REST work (depth snapshots, kline gap
// fills) runs in bounded background goroutines and re-enters under the lock.
// Duplicates caused by overlapping connections during a rotation are dropped
// by identity: depth by update id, klines by open time, trades by aggregate
// trade id, book tickers by update id.
package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
	"github.com/cembasaranoglu/trader/internal/marketdata/book"
)

// Bar is one closed kline.
type Bar struct {
	Symbol      string
	Interval    string
	OpenTime    time.Time
	CloseTime   time.Time
	Open        *big.Rat
	High        *big.Rat
	Low         *big.Rat
	Close       *big.Rat
	Volume      *big.Rat
	QuoteVolume *big.Rat
	Trades      int64
	FromREST    bool // filled from REST after a stream gap
	GapBefore   bool // bars before this one could not be recovered
}

// Trade is one aggregate trade.
type Trade struct {
	Symbol       string
	AggID        int64
	FirstTradeID int64
	LastTradeID  int64
	Price        *big.Rat
	Qty          *big.Rat
	Time         time.Time
	BuyerMaker   bool
}

// Ticker is the best bid and ask.
type Ticker struct {
	Symbol   string
	UpdateID int64
	BidPrice *big.Rat
	BidQty   *big.Rat
	AskPrice *big.Rat
	AskQty   *big.Rat
}

// Sink receives data in order. Calls are made with the Manager's lock held,
// so implementations must be fast and must not call back into the Manager.
type Sink interface {
	OnBar(Bar)
	OnTrade(Trade)
	OnTicker(Ticker)
}

// REST is the subset of the Binance REST client the Manager needs.
type REST interface {
	Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, error)
	KlinesRange(ctx context.Context, s exchange.Symbol, interval string, start, end time.Time, limit int) ([]exchange.Kline, error)
}

// Config configures a Manager.
type Config struct {
	Symbols        []exchange.Symbol // symbol rules (tick/step) from exchangeInfo
	Intervals      []string          // kline intervals to stream
	DepthLimit     int               // REST snapshot levels, 1..5000
	MaxDataAge     time.Duration     // R-9 freshness limit
	SnapshotSlots  int               // concurrent snapshot fetches; 0 → 2
	SnapshotDelay  time.Duration     // first delay before re-fetching a stale snapshot; 0 → 200ms
	SnapshotMaxDel time.Duration     // maximum re-fetch delay; 0 → 5s
	RESTTimeout    time.Duration     // per REST call; 0 → 10s
	GapFillRetries int               // kline gap fill attempts before giving up; 0 → 5
	GapFillBackoff time.Duration     // first retry delay, doubled each attempt; 0 → 1s
	Logger         *slog.Logger
	Now            func() time.Time
}

// Streams returns the combined-stream names for the configuration.
func (c Config) Streams() []string {
	var out []string
	for _, s := range c.Symbols {
		l := strings.ToLower(s.Name)
		out = append(out, l+"@depth@100ms", l+"@bookTicker", l+"@aggTrade")
		for _, iv := range c.Intervals {
			out = append(out, l+"@kline_"+iv)
		}
	}
	return out
}

// SymbolStats are per-symbol counters for monitoring and the observe report.
type SymbolStats struct {
	Book          book.Stats
	BookState     string
	BookUpdateID  int64
	SnapshotErrs  int64
	Bars          int64
	BarsFromREST  int64
	BarGapsUnfill int64
	Trades        int64
	TradeDupes    int64
	TradeGaps     int64 // missing aggregate trade ids (not refilled)
	Tickers       int64
	TickerDupes   int64
	LastEvent     time.Time
}

type klineTrack struct {
	last    time.Time // open time of the last published bar
	pending []*ws.KlineEvent
	filling bool
}

type symState struct {
	sym       exchange.Symbol
	book      *book.Book
	snapRetry int       // consecutive snapshots that did not leave the book live for long
	liveSince time.Time // when the book last became live
	klines    map[string]*klineTrack
	lastAgg   int64
	lastTkr   int64
	stats     SymbolStats
}

// Manager owns market-data state. Create it with New and feed it with
// Handle (as a ws.Handler).
type Manager struct {
	cfg  Config
	rest REST
	sink Sink
	log  *slog.Logger
	now  func() time.Time
	sem  chan struct{}

	mu      sync.Mutex
	ctx     context.Context
	symbols map[string]*symState // by upper-case symbol name
	wg      sync.WaitGroup
}

// ErrUnknownInterval reports an interval with no fixed successor rule.
var ErrUnknownInterval = errors.New("marketdata: unsupported interval")

// New validates cfg and returns a Manager. ctx bounds background REST work.
func New(ctx context.Context, cfg Config, rest REST, sink Sink) (*Manager, error) {
	if len(cfg.Symbols) == 0 {
		return nil, errors.New("marketdata: no symbols")
	}
	if cfg.DepthLimit < 1 || cfg.DepthLimit > 5000 {
		return nil, fmt.Errorf("marketdata: depth limit %d not in [1, 5000]", cfg.DepthLimit)
	}
	if cfg.MaxDataAge <= 0 {
		return nil, errors.New("marketdata: MaxDataAge must be positive")
	}
	for _, iv := range cfg.Intervals {
		if _, err := NextOpen(iv, time.Unix(0, 0).UTC()); err != nil {
			return nil, err
		}
	}
	if cfg.SnapshotSlots <= 0 {
		cfg.SnapshotSlots = 2
	}
	if cfg.SnapshotDelay <= 0 {
		cfg.SnapshotDelay = 200 * time.Millisecond
	}
	if cfg.SnapshotMaxDel <= 0 {
		cfg.SnapshotMaxDel = 5 * time.Second
	}
	if cfg.RESTTimeout <= 0 {
		cfg.RESTTimeout = 10 * time.Second
	}
	if cfg.GapFillRetries <= 0 {
		cfg.GapFillRetries = 5
	}
	if cfg.GapFillBackoff <= 0 {
		cfg.GapFillBackoff = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{cfg: cfg, rest: rest, sink: sink, log: cfg.Logger, now: cfg.Now,
		sem: make(chan struct{}, cfg.SnapshotSlots), ctx: ctx, symbols: map[string]*symState{}}
	for _, s := range cfg.Symbols {
		st := &symState{sym: s, book: book.New(s, 0), klines: map[string]*klineTrack{}}
		for _, iv := range cfg.Intervals {
			st.klines[iv] = &klineTrack{}
		}
		m.symbols[strings.ToUpper(s.Name)] = st
	}
	return m, nil
}

// Wait blocks until background REST work has finished. Call it after ctx is
// done and after the stream (Handle) and RetrySnapshots have stopped.
func (m *Manager) Wait() { m.wg.Wait() }

// Handle processes one stream message. It is a ws.Handler.
func (m *Manager) Handle(msg ws.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev := msg.Event.(type) {
	case *ws.DepthEvent:
		st := m.symbols[ev.Symbol]
		if st == nil {
			return
		}
		st.stats.LastEvent = msg.Received
		if st.book.OnDepth(ev) {
			m.fetchSnapshot(st)
		}
	case *ws.BookTickerEvent:
		st := m.symbols[ev.Symbol]
		if st == nil {
			return
		}
		st.stats.LastEvent = msg.Received
		if ev.UpdateID <= st.lastTkr {
			st.stats.TickerDupes++
			return
		}
		t, err := tickerFrom(ev)
		if err != nil {
			m.log.Warn("bad bookTicker", "symbol", ev.Symbol, "err", err)
			return
		}
		st.lastTkr = ev.UpdateID
		st.stats.Tickers++
		m.sink.OnTicker(t)
	case *ws.AggTradeEvent:
		st := m.symbols[ev.Symbol]
		if st == nil {
			return
		}
		st.stats.LastEvent = msg.Received
		if ev.AggID <= st.lastAgg {
			st.stats.TradeDupes++
			return
		}
		tr, err := tradeFrom(ev)
		if err != nil {
			m.log.Warn("bad aggTrade", "symbol", ev.Symbol, "err", err)
			return
		}
		if st.lastAgg != 0 && ev.AggID > st.lastAgg+1 {
			st.stats.TradeGaps += ev.AggID - st.lastAgg - 1
		}
		st.lastAgg = ev.AggID
		st.stats.Trades++
		m.sink.OnTrade(tr)
	case *ws.KlineEvent:
		st := m.symbols[ev.Symbol]
		if st == nil {
			return
		}
		st.stats.LastEvent = msg.Received
		if !ev.Closed {
			return
		}
		kt := st.klines[ev.Interval]
		if kt == nil {
			return
		}
		m.onClosedKline(st, kt, ev)
	}
}

// RetrySnapshots re-requests snapshots for books whose last request failed.
// Call it periodically (for example every second).
func (m *Manager) RetrySnapshots() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.symbols {
		if st.book.Retry() {
			m.fetchSnapshot(st)
		}
	}
}

// fetchSnapshot fetches a depth snapshot in the background. Consecutive
// fetches that do not make the book live (stale snapshot, gap, error) are
// delayed exponentially from SnapshotDelay up to SnapshotMaxDel, so a REST
// snapshot that lags the stream cannot cause a tight request loop.
// It must be called with m.mu held.
func (m *Manager) fetchSnapshot(st *symState) {
	// A book that stayed live for a while earns a fresh retry budget; one that
	// keeps breaking right after a snapshot (bad values, crossed) backs off.
	if st.snapRetry > 0 && !st.liveSince.IsZero() && m.now().Sub(st.liveSince) >= snapshotHealthy {
		st.snapRetry = 0
	}
	var delay time.Duration
	if st.snapRetry > 0 {
		delay = m.cfg.SnapshotDelay << min(st.snapRetry-1, 16)
		if delay > m.cfg.SnapshotMaxDel || delay <= 0 {
			delay = m.cfg.SnapshotMaxDel
		}
	}
	st.snapRetry++
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-m.ctx.Done():
				t.Stop()
				m.mu.Lock()
				st.book.SnapshotFailed()
				m.mu.Unlock()
				return
			}
		}
		select {
		case m.sem <- struct{}{}:
		case <-m.ctx.Done():
			m.mu.Lock()
			st.book.SnapshotFailed()
			m.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(m.ctx, m.cfg.RESTTimeout)
		ob, err := m.rest.Depth(ctx, st.sym, m.cfg.DepthLimit)
		cancel()
		<-m.sem
		var wb *binance.WeightBudgetError
		if errors.As(err, &wb) && sleepCtx(m.ctx, wb.Wait) {
			// The local budget refused it: wait as instructed and retry
			// without counting a snapshot error.
			m.mu.Lock()
			st.book.SnapshotFailed()
			if st.book.Retry() {
				m.fetchSnapshotNow(st)
			}
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			st.stats.SnapshotErrs++
			st.book.SnapshotFailed()
			m.log.Warn("depth snapshot failed", "symbol", st.sym.Name, "err", err)
			return
		}
		need, err := st.book.ApplySnapshot(ob)
		if err != nil {
			m.log.Error("order book failed", "symbol", st.sym.Name, "err", err)
			return
		}
		if st.book.State() == book.Live {
			st.liveSince = m.now()
		}
		if need {
			m.fetchSnapshot(st)
		}
	}()
}

// snapshotHealthy is how long a book must stay live before its snapshot
// retry backoff resets.
const snapshotHealthy = time.Minute

// fetchSnapshotNow fetches without adding backoff (used after a budget
// wait, which already delayed the request). Call with m.mu held.
func (m *Manager) fetchSnapshotNow(st *symState) {
	saved := st.snapRetry
	st.snapRetry = 0
	m.fetchSnapshot(st)
	st.snapRetry = saved
}

// sleepCtx waits d or until ctx is done; it reports whether d elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (m *Manager) onClosedKline(st *symState, kt *klineTrack, ev *ws.KlineEvent) {
	if !kt.last.IsZero() && !ev.OpenTime.After(kt.last) {
		return // duplicate or older
	}
	if kt.filling {
		kt.pending = append(kt.pending, ev)
		return
	}
	if !kt.last.IsZero() {
		next, _ := NextOpen(ev.Interval, kt.last)
		if ev.OpenTime.After(next) {
			kt.filling = true
			kt.pending = append(kt.pending, ev)
			m.fillGap(st, kt, ev.Interval, next, ev.OpenTime.Add(-time.Millisecond))
			return
		}
	}
	m.publishWS(st, kt, ev, false)
}

func (m *Manager) publishWS(st *symState, kt *klineTrack, ev *ws.KlineEvent, gapBefore bool) {
	bar, err := barFromEvent(ev)
	if err != nil {
		m.log.Warn("bad kline", "symbol", ev.Symbol, "err", err)
		return
	}
	bar.GapBefore = gapBefore
	kt.last = ev.OpenTime
	st.stats.Bars++
	m.sink.OnBar(bar)
}

// fillGap fetches bars with open time in [from, to] (paging by 1000) and then
// publishes the pending stream bars. Each page is retried with exponential
// backoff; after GapFillRetries failures the bars recovered so far are
// published and the next bar is marked GapBefore.
func (m *Manager) fillGap(st *symState, kt *klineTrack, interval string, from, to time.Time) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		bars, err := m.fetchRange(st.sym, interval, from, to)
		m.mu.Lock()
		defer m.mu.Unlock()
		kt.filling = false
		if m.ctx.Err() != nil {
			kt.pending = nil // shutting down: no partial publication, no false gap
			return
		}
		gap := err != nil
		if err != nil {
			m.log.Warn("kline gap fill failed", "symbol", st.sym.Name, "interval", interval, "err", err)
		}
		expect := from
		for _, k := range bars {
			if !k.OpenTime.After(kt.last) || k.OpenTime.After(to) {
				continue
			}
			b := barFromKline(st.sym.Name, interval, k)
			b.FromREST, b.GapBefore = true, !k.OpenTime.Equal(expect)
			if b.GapBefore {
				gap = true
			}
			kt.last = k.OpenTime
			st.stats.Bars++
			st.stats.BarsFromREST++
			m.sink.OnBar(b)
			expect, _ = NextOpen(interval, k.OpenTime)
		}
		if !expect.After(to) {
			gap = true // bars between the last recovered one and the next stream bar are missing
		}
		if gap {
			st.stats.BarGapsUnfill++
		}
		pending := kt.pending
		kt.pending = nil
		first := true
		for i, ev := range pending {
			if !ev.OpenTime.After(kt.last) {
				continue
			}
			// Another gap inside the bars that arrived during the fill:
			// fill it before publishing the rest.
			if next, _ := NextOpen(interval, kt.last); ev.OpenTime.After(next) && !(gap && first) {
				kt.filling = true
				kt.pending = append([]*ws.KlineEvent(nil), pending[i:]...)
				m.fillGap(st, kt, interval, next, ev.OpenTime.Add(-time.Millisecond))
				return
			}
			m.publishWS(st, kt, ev, gap && first)
			first = false
		}
	}()
}

// fetchRange pages through KlinesRange until the range is covered, the
// exchange returns a short page, or retries are exhausted. It returns the
// bars fetched so far together with the last error.
func (m *Manager) fetchRange(sym exchange.Symbol, interval string, from, to time.Time) ([]exchange.Kline, error) {
	const pageSize = 1000
	var all []exchange.Kline
	start := from
	for !start.After(to) {
		var page []exchange.Kline
		var err error
		delay := m.cfg.GapFillBackoff
		for attempt := 0; attempt < m.cfg.GapFillRetries; attempt++ {
			ctx, cancel := context.WithTimeout(m.ctx, m.cfg.RESTTimeout)
			page, err = m.rest.KlinesRange(ctx, sym, interval, start, to, pageSize)
			cancel()
			var wb *binance.WeightBudgetError
			if errors.As(err, &wb) {
				// Refused locally by the weight budget: wait as instructed;
				// this does not use up an attempt.
				if !sleepCtx(m.ctx, wb.Wait) {
					return all, m.ctx.Err()
				}
				attempt--
				continue
			}
			if err == nil || m.ctx.Err() != nil || attempt == m.cfg.GapFillRetries-1 {
				break
			}
			t := time.NewTimer(delay)
			select {
			case <-m.ctx.Done():
			case <-t.C:
			}
			t.Stop()
			delay *= 2
		}
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
		next, _ := NextOpen(interval, page[len(page)-1].OpenTime)
		if !next.After(start) {
			return all, errors.New("kline paging did not advance")
		}
		start = next
	}
	return all, nil
}

// Stale reports whether the symbol's data is older than MaxDataAge (R-9),
// including when its order book is not live. Unknown symbols are stale.
func (m *Manager) Stale(symbol string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.symbols[strings.ToUpper(symbol)]
	if st == nil {
		return true
	}
	return m.staleLocked(st)
}

func (m *Manager) staleLocked(st *symState) bool {
	if st.book.State() != book.Live || st.stats.LastEvent.IsZero() {
		return true
	}
	return m.now().Sub(st.stats.LastEvent) > m.cfg.MaxDataAge
}

// BookView is a read-only copy of the top of a book with its freshness,
// taken under one lock so the levels and the flag agree.
type BookView struct {
	book.View
	Stale bool // R-9: no event within MaxDataAge, or the book is not live
}

// Book returns up to n levels per side of the symbol's order book.
func (m *Manager) Book(symbol string, n int) (BookView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.symbols[strings.ToUpper(symbol)]
	if st == nil {
		return BookView{}, false
	}
	return BookView{View: st.book.View(n), Stale: m.staleLocked(st)}, true
}

// Stats returns a copy of the per-symbol counters, keyed by symbol.
func (m *Manager) Stats() map[string]SymbolStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]SymbolStats, len(m.symbols))
	for name, st := range m.symbols {
		s := st.stats
		s.Book = st.book.Stats()
		s.BookState = st.book.State().String()
		s.BookUpdateID = st.book.LastUpdateID()
		out[name] = s
	}
	return out
}

// Symbols returns the managed symbol names in sorted order.
func (m *Manager) Symbols() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.symbols))
	for n := range m.symbols {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
