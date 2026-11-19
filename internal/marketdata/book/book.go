// Package book maintains a local order book from a REST depth snapshot and
// the diff-depth stream, following "How to manage a local order book
// correctly" (binance-spot-api-docs, web-socket-streams.md):
//
//  1. Buffer stream events; note U of the first event.
//  2. Fetch a snapshot. If its lastUpdateId < that first U, fetch again.
//  3. Drop buffered events with u <= lastUpdateId; the first remaining event
//     must contain lastUpdateId+1 (U <= lastUpdateId+1).
//  4. Apply events: ignore u < local id; U > local id + 1 means events were
//     missed, so discard the book and restart; quantity 0 removes a level;
//     set the local id to u.
//
// Additionally a crossed book (best bid >= best ask) is treated as corrupt
// and rebuilt. Prices and quantities are int64 fixed-point values; the scale
// comes from the symbol's tickSize/stepSize, or from the data when a filter is
// disabled. A value finer than the scale raises the scale and rebuilds; a
// value that overflows int64 at the required scale puts the book in the
// Failed state until it is recreated.
//
// A Book is not safe for concurrent use; the owner serializes access.
package book

import (
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
	"github.com/cembasaranoglu/trader/internal/fixedpoint"
)

// State is the synchronization state of a book.
type State int

// Book states.
const (
	Syncing State = iota // buffering events, waiting for a snapshot
	Live                 // consistent with the exchange
	Failed               // unrecoverable (int64 overflow); recreate the book
)

func (s State) String() string {
	switch s {
	case Syncing:
		return "syncing"
	case Live:
		return "live"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// DefaultMaxBuffer bounds buffered events while waiting for a snapshot.
const DefaultMaxBuffer = 10000

// Stats are cumulative counters.
type Stats struct {
	Applied      int64 // events applied to a live book
	Ignored      int64 // stale or duplicate events (u < local id)
	Gaps         int64 // sequence gaps detected (U > local id + 1)
	Crossed      int64 // crossed books detected
	ScaleRaises  int64 // rebuilds because a value was finer than the scale
	BufferResets int64 // buffer overflows while syncing
	Snapshots    int64 // snapshots applied
	StaleSnaps   int64 // snapshots older than the first buffered event
	BadValues    int64 // events with unparsable prices or quantities
}

// Book is one symbol's local order book.
type Book struct {
	symbol string
	pScale int
	qScale int
	maxBuf int

	state     State
	bids      map[int64]int64
	asks      map[int64]int64
	lastID    int64
	buf       []*ws.DepthEvent
	requested bool // a snapshot request is outstanding
	failure   error
	stats     Stats
}

// New returns a book in the Syncing state. maxBuf <= 0 selects DefaultMaxBuffer.
func New(sym exchange.Symbol, maxBuf int) *Book {
	if maxBuf <= 0 {
		maxBuf = DefaultMaxBuffer
	}
	b := &Book{symbol: sym.Name, maxBuf: maxBuf}
	if sym.Filters.TickSize != nil {
		if s, err := fixedpoint.ScaleOf(sym.Filters.TickSize); err == nil {
			b.pScale = s
		}
	}
	if sym.Filters.StepSize != nil {
		if s, err := fixedpoint.ScaleOf(sym.Filters.StepSize); err == nil {
			b.qScale = s
		}
	}
	b.reset()
	return b
}

func (b *Book) reset() {
	b.state = Syncing
	b.bids = map[int64]int64{}
	b.asks = map[int64]int64{}
	b.lastID = 0
	b.buf = b.buf[:0]
}

// Symbol returns the symbol name.
func (b *Book) Symbol() string { return b.symbol }

// State returns the synchronization state.
func (b *Book) State() State { return b.state }

// Failure returns the error that moved the book to Failed, if any.
func (b *Book) Failure() error { return b.failure }

// Stats returns the counters.
func (b *Book) Stats() Stats { return b.stats }

// LastUpdateID returns the local update id (0 while syncing).
func (b *Book) LastUpdateID() int64 { return b.lastID }

// OnDepth processes one diff-depth event. It returns true when the caller
// must fetch a snapshot and pass it to ApplySnapshot.
func (b *Book) OnDepth(ev *ws.DepthEvent) bool {
	switch b.state {
	case Failed:
		return false
	case Syncing:
		return b.buffer(ev)
	}
	if ev.FinalUpdateID < b.lastID {
		b.stats.Ignored++
		return false
	}
	if ev.FirstUpdateID > b.lastID+1 {
		b.stats.Gaps++
		return b.resync(ev)
	}
	if err := b.apply(ev); err != nil {
		return b.handleApplyError(err, ev)
	}
	b.stats.Applied++
	if b.crossed() {
		b.stats.Crossed++
		return b.resync(nil)
	}
	return false
}

// resync discards the book and starts buffering again, optionally with ev
// as the first buffered event.
func (b *Book) resync(ev *ws.DepthEvent) bool {
	b.reset()
	if ev != nil {
		return b.buffer(ev)
	}
	return b.request()
}

func (b *Book) request() bool {
	if b.requested {
		return false
	}
	b.requested = true
	return true
}

func (b *Book) buffer(ev *ws.DepthEvent) bool {
	if len(b.buf) >= b.maxBuf {
		b.stats.BufferResets++
		b.buf = b.buf[:0]
	}
	b.buf = append(b.buf, ev)
	return b.request()
}

// SnapshotFailed tells the book that an outstanding snapshot request ended
// without a snapshot; the next event (or Retry) requests a new one.
func (b *Book) SnapshotFailed() { b.requested = false }

// NeedsSnapshot reports whether the book is syncing with no request
// outstanding (for example after SnapshotFailed) and should be retried.
func (b *Book) NeedsSnapshot() bool {
	return b.state == Syncing && !b.requested
}

// Retry marks a new snapshot request as outstanding when NeedsSnapshot is true.
func (b *Book) Retry() bool {
	if !b.NeedsSnapshot() {
		return false
	}
	return b.request()
}

// ApplySnapshot installs a REST depth snapshot. It returns true when another
// snapshot is needed (the snapshot was older than the buffered events, a gap
// remained, or the scale had to be raised).
func (b *Book) ApplySnapshot(ob exchange.OrderBook) (bool, error) {
	b.requested = false
	if b.state != Syncing {
		return false, nil
	}
	if len(b.buf) > 0 && ob.LastUpdateID < b.buf[0].FirstUpdateID {
		b.stats.StaleSnaps++
		return b.request(), nil
	}
	if err := b.adoptScales(ob); err != nil {
		return false, b.fail(err)
	}
	bids, err := b.levelsFromSnapshot(ob.Bids)
	if err != nil {
		return false, b.fail(err)
	}
	asks, err := b.levelsFromSnapshot(ob.Asks)
	if err != nil {
		return false, b.fail(err)
	}
	pending := slices.Clone(b.buf)
	b.buf = b.buf[:0]
	b.bids, b.asks, b.lastID, b.state = bids, asks, ob.LastUpdateID, Live
	b.stats.Snapshots++
	first := true
	for i, ev := range pending {
		if ev.FinalUpdateID <= ob.LastUpdateID {
			continue
		}
		var need bool
		if first && ev.FirstUpdateID > ob.LastUpdateID+1 {
			b.stats.Gaps++
			need = b.resync(ev)
		} else {
			need = b.OnDepth(ev)
		}
		first = false
		if b.state == Failed {
			return false, b.failure
		}
		if b.state == Syncing {
			// Keep the events after the one that broke continuity buffered.
			for _, rest := range pending[i+1:] {
				if b.buffer(rest) {
					need = true
				}
			}
			return need, nil
		}
	}
	if b.crossed() {
		b.stats.Crossed++
		return b.resync(nil), nil
	}
	return false, nil
}

func (b *Book) fail(err error) error {
	b.reset()
	b.state = Failed
	b.failure = fmt.Errorf("book %s: %w", b.symbol, err)
	return b.failure
}

// adoptScales derives scales from the snapshot when a filter is disabled
// and raises them when the snapshot holds finer values.
func (b *Book) adoptScales(ob exchange.OrderBook) error {
	for _, side := range [][]exchange.Level{ob.Bids, ob.Asks} {
		for _, l := range side {
			ps, err := fixedpoint.ScaleOf(l.Price)
			if err != nil {
				return err
			}
			qs, err := fixedpoint.ScaleOf(l.Qty)
			if err != nil {
				return err
			}
			if ps > b.pScale {
				b.pScale = ps
			}
			if qs > b.qScale {
				b.qScale = qs
			}
		}
	}
	return nil
}

func (b *Book) levelsFromSnapshot(ls []exchange.Level) (map[int64]int64, error) {
	m := make(map[int64]int64, len(ls))
	for _, l := range ls {
		p, err := fixedpoint.FromRat(l.Price, b.pScale)
		if err != nil {
			return nil, err
		}
		q, err := fixedpoint.FromRat(l.Qty, b.qScale)
		if err != nil {
			return nil, err
		}
		if q > 0 {
			m[p] = q
		}
	}
	return m, nil
}

// scaleError carries the scale a value needs.
type scaleError struct {
	price bool
	need  int
}

func (e *scaleError) Error() string { return fmt.Sprintf("value needs scale %d", e.need) }

func (b *Book) parse(s string, price bool) (int64, error) {
	scale := b.qScale
	if price {
		scale = b.pScale
	}
	v, err := fixedpoint.Parse(s, scale)
	if errors.Is(err, fixedpoint.ErrScale) {
		need, derr := fixedpoint.Decimals(s)
		if derr != nil || need > fixedpoint.MaxScale {
			return 0, fixedpoint.ErrScale
		}
		return 0, &scaleError{price: price, need: need}
	}
	return v, err
}

// apply validates every level of ev before changing the book, so a bad event
// never leaves the book half-updated.
func (b *Book) apply(ev *ws.DepthEvent) error {
	type upd struct {
		side map[int64]int64
		p, q int64
	}
	ups := make([]upd, 0, len(ev.Bids)+len(ev.Asks))
	for _, side := range []struct {
		levels [][2]string
		m      map[int64]int64
	}{{ev.Bids, b.bids}, {ev.Asks, b.asks}} {
		for _, l := range side.levels {
			p, err := b.parse(l[0], true)
			if err != nil {
				return err
			}
			q, err := b.parse(l[1], false)
			if err != nil {
				return err
			}
			ups = append(ups, upd{side.m, p, q})
		}
	}
	for _, u := range ups {
		if u.q == 0 {
			delete(u.side, u.p)
		} else {
			u.side[u.p] = u.q
		}
	}
	b.lastID = ev.FinalUpdateID
	return nil
}

func (b *Book) handleApplyError(err error, ev *ws.DepthEvent) bool {
	var se *scaleError
	if errors.As(err, &se) {
		b.stats.ScaleRaises++
		if se.price {
			b.pScale = se.need
		} else {
			b.qScale = se.need
		}
		return b.resync(ev)
	}
	if errors.Is(err, fixedpoint.ErrOverflow) {
		_ = b.fail(err)
		return false
	}
	// Any other invalid value: the stream data cannot be trusted; rebuild.
	b.stats.BadValues++
	return b.resync(nil)
}

func (b *Book) best() (bid, ask int64, okBid, okAsk bool) {
	for p := range b.bids {
		if !okBid || p > bid {
			bid, okBid = p, true
		}
	}
	for p := range b.asks {
		if !okAsk || p < ask {
			ask, okAsk = p, true
		}
	}
	return
}

func (b *Book) crossed() bool {
	bid, ask, okB, okA := b.best()
	return okB && okA && bid >= ask
}

// Level is one price level as exact decimals.
type Level struct {
	Price *big.Rat
	Qty   *big.Rat
}

// View is a read-only copy of the top of the book.
type View struct {
	Symbol       string
	State        State
	LastUpdateID int64
	Bids         []Level // best first
	Asks         []Level // best first
}

// View returns up to n levels per side (n <= 0: all).
func (b *Book) View(n int) View {
	v := View{Symbol: b.symbol, State: b.state, LastUpdateID: b.lastID}
	if b.state != Live {
		return v
	}
	v.Bids = b.side(b.bids, n, true)
	v.Asks = b.side(b.asks, n, false)
	return v
}

func (b *Book) side(m map[int64]int64, n int, desc bool) []Level {
	keys := make([]int64, 0, len(m))
	for p := range m {
		keys = append(keys, p)
	}
	slices.Sort(keys)
	if desc {
		slices.Reverse(keys)
	}
	if n > 0 && len(keys) > n {
		keys = keys[:n]
	}
	out := make([]Level, 0, len(keys))
	for _, p := range keys {
		out = append(out, Level{Price: fixedpoint.ToRat(p, b.pScale), Qty: fixedpoint.ToRat(m[p], b.qScale)})
	}
	return out
}
