package book

import (
	"fmt"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
	"testing"
)

// BenchmarkOnDepth applies 100ms diff events (10 bid and 10 ask changes, one
// of them removing the best level) to a live 1000-level book: the work done
// per depth message on the stream.
func BenchmarkOnDepth(b *testing.B) {
	var bids, asks [][2]string
	for i := range 1000 {
		bids = append(bids, [2]string{fmt.Sprintf("%d.00", 90000-i), "0.5"})
		asks = append(asks, [2]string{fmt.Sprintf("%d.00", 90001+i), "0.5"})
	}
	bk := New(sym(), 0)
	bk.OnDepth(ev(101, 101, nil, nil))
	if need, err := bk.ApplySnapshot(snap(101, bids, asks)); err != nil || need || bk.State() != Live {
		b.Fatalf("snapshot: need=%v state=%v err=%v", need, bk.State(), err)
	}
	events := make([]*ws.DepthEvent, 64)
	for k := range events {
		var eb, ea [][2]string
		for i := range 10 {
			q := "0.7"
			if i == 0 && k%2 == 0 {
				q = "0" // remove the best level, then restore it on the next event
			}
			eb = append(eb, [2]string{fmt.Sprintf("%d.00", 90000-i), q})
			ea = append(ea, [2]string{fmt.Sprintf("%d.00", 90001+i), q})
		}
		events[k] = &ws.DepthEvent{Symbol: "BTCUSDT", Bids: eb, Asks: ea}
	}
	id := int64(102)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := events[i%len(events)]
		e.FirstUpdateID, e.FinalUpdateID = id, id
		id++
		if bk.OnDepth(e) || bk.State() != Live {
			b.Fatalf("book left the live state at event %d: state=%v stats=%+v err=%v", i, bk.State(), bk.Stats(), bk.Failure())
		}
	}
}
