package book

import (
	"fmt"
	"testing"
)

// FuzzBookInvariants drives a book with arbitrary event and snapshot
// sequences. Invariants: no panic; a Live book is never crossed and its update
// id never decreases while it stays Live.
func FuzzBookInvariants(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	f.Add([]byte{0, 255, 3, 128, 7, 7, 7, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		b := New(sym(), 8)
		var id int64 = 100
		prevLive := int64(-1)
		for i := 0; i+2 < len(data); i += 3 {
			op, x, y := data[i], int64(data[i+1]), int64(data[i+2])
			price := func(v int64) string { return fmt.Sprintf("%d.%02d", 90+v%20, v%100) }
			qty := fmt.Sprintf("%d", y%4)
			switch op % 4 {
			case 0, 1: // event with possible gaps or duplicates
				U := id + x%5 - 2
				u := U + y%3
				if u < U {
					u = U
				}
				id = u
				side := [][2]string{{price(x), qty}}
				if op%2 == 0 {
					b.OnDepth(ev(U, u, side, nil))
				} else {
					b.OnDepth(ev(U, u, nil, side))
				}
			case 2: // snapshot
				b.ApplySnapshot(snap(id-x%4, [][2]string{{"90.00", "1"}}, [][2]string{{"110.00", "1"}}))
			case 3:
				b.SnapshotFailed()
				b.Retry()
			}
			if b.State() == Live {
				// The cached best prices must equal a full scan.
				bid, ask, okB, okA := b.best()
				var sb, sa int64
				var hb, ha bool
				for p := range b.bids {
					if !hb || p > sb {
						sb, hb = p, true
					}
				}
				for p := range b.asks {
					if !ha || p < sa {
						sa, ha = p, true
					}
				}
				if okB != hb || okA != ha || (hb && bid != sb) || (ha && ask != sa) {
					t.Fatalf("cached best %d/%d (%v/%v) != scan %d/%d (%v/%v)", bid, ask, okB, okA, sb, sa, hb, ha)
				}
				if b.crossed() {
					t.Fatal("live book is crossed")
				}
				if prevLive >= 0 && b.LastUpdateID() < prevLive {
					t.Fatalf("update id went back: %d < %d", b.LastUpdateID(), prevLive)
				}
				prevLive = b.LastUpdateID()
			} else {
				prevLive = -1
			}
		}
	})
}
