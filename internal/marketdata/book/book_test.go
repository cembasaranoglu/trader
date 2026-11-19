package book

import (
	"errors"
	"math/big"
	"testing"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
	"github.com/cembasaranoglu/trader/internal/fixedpoint"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func sym() exchange.Symbol {
	return exchange.Symbol{Name: "BTCUSDT", Filters: exchange.Filters{TickSize: rat("0.01"), StepSize: rat("0.00001")}}
}

func snap(id int64, bids, asks [][2]string) exchange.OrderBook {
	ob := exchange.OrderBook{LastUpdateID: id}
	for _, l := range bids {
		ob.Bids = append(ob.Bids, exchange.Level{Price: rat(l[0]), Qty: rat(l[1])})
	}
	for _, l := range asks {
		ob.Asks = append(ob.Asks, exchange.Level{Price: rat(l[0]), Qty: rat(l[1])})
	}
	return ob
}

func ev(U, u int64, bids, asks [][2]string) *ws.DepthEvent {
	return &ws.DepthEvent{Symbol: "BTCUSDT", FirstUpdateID: U, FinalUpdateID: u, Bids: bids, Asks: asks}
}

var (
	baseBids = [][2]string{{"100.00", "1"}, {"99.99", "2"}}
	baseAsks = [][2]string{{"100.01", "1"}, {"100.02", "3"}}
)

func liveBook(t *testing.T) *Book {
	t.Helper()
	b := New(sym(), 0)
	if !b.OnDepth(ev(101, 101, nil, nil)) {
		t.Fatal("first event must request a snapshot")
	}
	if need, err := b.ApplySnapshot(snap(101, baseBids, baseAsks)); need || err != nil || b.State() != Live {
		t.Fatalf("need=%v err=%v state=%s", need, err, b.State())
	}
	return b
}

func best(t *testing.T, b *Book) (string, string) {
	t.Helper()
	v := b.View(1)
	if len(v.Bids) == 0 || len(v.Asks) == 0 {
		t.Fatalf("empty side: %+v", v)
	}
	return v.Bids[0].Price.FloatString(2), v.Asks[0].Price.FloatString(2)
}

func TestSyncProcedure(t *testing.T) {
	t.Parallel()
	b := New(sym(), 0)
	if !b.OnDepth(ev(101, 103, [][2]string{{"100.00", "5"}}, nil)) {
		t.Fatal("first event must request a snapshot")
	}
	if b.OnDepth(ev(104, 106, [][2]string{{"99.98", "1"}}, nil)) {
		t.Fatal("only one outstanding request")
	}
	b.OnDepth(ev(107, 107, nil, [][2]string{{"100.01", "0"}}))
	// Snapshot at 104: drop u <= 104 (first event), apply 104..106 and 107.
	need, err := b.ApplySnapshot(snap(104, baseBids, baseAsks))
	if need || err != nil || b.State() != Live || b.LastUpdateID() != 107 {
		t.Fatalf("need=%v err=%v state=%s id=%d", need, err, b.State(), b.LastUpdateID())
	}
	v := b.View(0)
	if len(v.Bids) != 3 || v.Bids[0].Qty.Cmp(rat("1")) != 0 { // 100.00 keeps snapshot qty 1
		t.Fatalf("bids %+v", v.Bids)
	}
	if bid, ask := best(t, b); bid != "100.00" || ask != "100.02" { // 100.01 removed by qty 0
		t.Fatalf("best %s/%s", bid, ask)
	}
}

func TestSnapshotOlderThanFirstEvent(t *testing.T) {
	t.Parallel()
	b := New(sym(), 0)
	b.OnDepth(ev(200, 201, nil, nil))
	// The documented rule is "strictly less than U": 199 is stale even though
	// it would be contiguous.
	need, err := b.ApplySnapshot(snap(199, baseBids, baseAsks))
	if !need || err != nil || b.State() != Syncing || b.Stats().StaleSnaps != 1 {
		t.Fatalf("stale snapshot must be refetched: need=%v err=%v state=%s", need, err, b.State())
	}
}

func TestGapAfterSnapshot(t *testing.T) {
	t.Parallel()
	b := New(sym(), 0)
	b.OnDepth(ev(110, 112, nil, nil))
	b.OnDepth(ev(120, 121, nil, nil))
	b.OnDepth(ev(122, 125, nil, nil))
	// Snapshot 113 drops the first event; the next one starts at 120 > 114 → gap.
	need, err := b.ApplySnapshot(snap(113, baseBids, baseAsks))
	if err != nil || b.State() != Syncing || !need || b.Stats().Gaps != 1 {
		t.Fatalf("need=%v err=%v state=%s gaps=%d", need, err, b.State(), b.Stats().Gaps)
	}
	// The events after the gap stay buffered; a snapshot at 121 applies 122..125.
	if need, err := b.ApplySnapshot(snap(121, baseBids, baseAsks)); need || err != nil || b.LastUpdateID() != 125 {
		t.Fatalf("need=%v err=%v id=%d", need, err, b.LastUpdateID())
	}
}

func TestLiveSequencing(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	if b.OnDepth(ev(90, 99, [][2]string{{"1.00", "1"}}, nil)) || b.Stats().Ignored != 1 {
		t.Fatal("old event must be ignored")
	}
	if b.OnDepth(ev(102, 103, [][2]string{{"99.98", "4"}}, nil)) || b.LastUpdateID() != 103 {
		t.Fatal("contiguous event must apply")
	}
	// Duplicate delivery from an overlapping connection: same range again.
	b.OnDepth(ev(102, 103, [][2]string{{"99.98", "4"}}, nil))
	if v := b.View(0); len(v.Bids) != 3 {
		t.Fatalf("duplicate must not change the book: %+v", v.Bids)
	}
	if !b.OnDepth(ev(110, 111, nil, nil)) || b.State() != Syncing || b.Stats().Gaps != 1 {
		t.Fatal("gap must restart sync")
	}
}

func TestCrossedBookRebuilds(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	if !b.OnDepth(ev(102, 102, [][2]string{{"100.05", "1"}}, nil)) || b.State() != Syncing || b.Stats().Crossed != 1 {
		t.Fatalf("crossed book must rebuild: state=%s", b.State())
	}
}

func TestScaleRaise(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	if !b.OnDepth(ev(102, 102, [][2]string{{"99.995", "1"}}, nil)) || b.State() != Syncing || b.Stats().ScaleRaises != 1 {
		t.Fatal("finer price must raise the scale and resync")
	}
	bids := append([][2]string{{"99.995", "1"}}, baseBids...)
	if need, err := b.ApplySnapshot(snap(102, bids, baseAsks)); need || err != nil {
		t.Fatalf("need=%v err=%v", need, err)
	}
	if v := b.View(0); len(v.Bids) != 3 || v.Bids[1].Price.Cmp(rat("99.995")) != 0 {
		t.Fatalf("raised-scale level missing: %+v", v.Bids)
	}
}

func TestOverflowFails(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	if b.OnDepth(ev(102, 102, nil, [][2]string{{"100.01", "999999999999999"}})) || b.State() != Failed {
		t.Fatalf("overflow must fail the book, state=%s", b.State())
	}
	if !errors.Is(b.Failure(), fixedpoint.ErrOverflow) {
		t.Fatalf("failure %v", b.Failure())
	}
	if b.OnDepth(ev(103, 103, nil, nil)) {
		t.Fatal("failed book must not request snapshots")
	}
}

func TestBadValueRebuilds(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	if !b.OnDepth(ev(102, 102, [][2]string{{"abc", "1"}}, nil)) || b.State() != Syncing || b.Stats().BadValues != 1 {
		t.Fatal("unparsable value must rebuild")
	}
}

func TestDisabledTickSizeUsesSnapshotScale(t *testing.T) {
	t.Parallel()
	s := sym()
	s.Filters.TickSize = nil // PRICE_FILTER tickSize 0 = disabled
	b := New(s, 0)
	b.OnDepth(ev(101, 101, nil, nil))
	if _, err := b.ApplySnapshot(snap(101, [][2]string{{"100.125", "1"}}, [][2]string{{"100.5", "1"}})); err != nil || b.State() != Live {
		t.Fatalf("err=%v state=%s", err, b.State())
	}
	if bid, _ := best(t, b); bid != "100.13" && bid != "100.12" {
		t.Fatalf("bid %s", bid)
	}
	if v := b.View(1); v.Bids[0].Price.Cmp(rat("100.125")) != 0 {
		t.Fatalf("exact price lost: %s", v.Bids[0].Price)
	}
}

func TestBufferOverflowAndRetry(t *testing.T) {
	t.Parallel()
	b := New(sym(), 2)
	b.OnDepth(ev(1, 1, nil, nil))
	b.OnDepth(ev(2, 2, nil, nil))
	b.OnDepth(ev(3, 3, nil, nil))
	if b.Stats().BufferResets != 1 {
		t.Fatal("buffer overflow not counted")
	}
	b.SnapshotFailed()
	if !b.NeedsSnapshot() || !b.Retry() || b.Retry() {
		t.Fatal("retry must request exactly once")
	}
	if need, err := b.ApplySnapshot(snap(3, baseBids, baseAsks)); need || err != nil || b.LastUpdateID() != 3 {
		t.Fatalf("need=%v err=%v id=%d", need, err, b.LastUpdateID())
	}
	if need, err := b.ApplySnapshot(snap(9, nil, nil)); need || err != nil || b.LastUpdateID() != 3 {
		t.Fatal("snapshot on a live book must be ignored")
	}
}

func TestViewOrderAndLimit(t *testing.T) {
	t.Parallel()
	b := liveBook(t)
	v := b.View(1)
	if len(v.Bids) != 1 || len(v.Asks) != 1 || v.Bids[0].Price.Cmp(rat("100")) != 0 || v.Asks[0].Price.Cmp(rat("100.01")) != 0 {
		t.Fatalf("%+v", v)
	}
	all := b.View(0)
	if all.Bids[0].Price.Cmp(all.Bids[1].Price) <= 0 || all.Asks[0].Price.Cmp(all.Asks[1].Price) >= 0 {
		t.Fatal("sides not ordered best first")
	}
	if New(sym(), 0).View(5).Bids != nil {
		t.Fatal("syncing book must expose no levels")
	}
}
