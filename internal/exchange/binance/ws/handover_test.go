package ws

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// hub models the exchange: one event sequence, delivered to every connection
// from the moment it subscribes, each connection with its own network lag.
type hub struct {
	mu   sync.Mutex
	subs map[chan hubEvent]struct{}
	seq  atomic.Int64
}

type hubEvent struct {
	id  int64
	gen time.Time
}

func newHub(ctx context.Context, every time.Duration) *hub {
	h := &hub{subs: map[chan hubEvent]struct{}{}}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				ev := hubEvent{id: h.seq.Add(1), gen: now}
				h.mu.Lock()
				for ch := range h.subs {
					select {
					case ch <- ev:
					default: // a stuck subscriber loses data, like a real slow consumer
					}
				}
				h.mu.Unlock()
			}
		}
	}()
	return h
}

func (h *hub) subscribe() (chan hubEvent, func()) {
	ch := make(chan hubEvent, 4096)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() { h.mu.Lock(); delete(h.subs, ch); h.mu.Unlock() }
}

// seqChecker records the first delivery of each id and counts ids that were
// skipped (a consumer that drops ids <= the last one seen, as marketdata does).
type seqChecker struct {
	mu   sync.Mutex
	last int64
	gaps int64
	dups int64
}

func (s *seqChecker) handle(m Message) {
	ev, ok := m.Event.(*AggTradeEvent)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case ev.AggID <= s.last:
		s.dups++
	default:
		if s.last != 0 && ev.AggID > s.last+1 {
			s.gaps += ev.AggID - s.last - 1
		}
		s.last = ev.AggID
	}
}

// TestHandoverLosesNothingWhenOldConnectionLags rotates between connections
// whose lags alternate (odd connections 10ms behind). Without a handover
// grace, switching from a lagging connection to a fresh one skips the
// events still in flight on the old one. The grace (150ms) is 15 times the
// lag so that a loaded CI machine does not make the test flaky.
func TestHandoverLosesNothingWhenOldConnectionLags(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub(ctx, time.Millisecond)
	fs := newFakeServer(t, func(rctx context.Context, idx int, c *websocket.Conn) {
		rctx = c.CloseRead(rctx)
		ch, unsub := h.subscribe()
		defer unsub()
		lag := time.Duration(0)
		if idx%2 == 1 {
			lag = 10 * time.Millisecond
		}
		for {
			select {
			case <-rctx.Done():
				return
			case ev := <-ch:
				if d := time.Until(ev.gen.Add(lag)); d > 0 {
					time.Sleep(d)
				}
				if send(rctx, c, fmt.Sprintf(aggTradeMsg, ev.id)) != nil {
					return
				}
			}
		}
	})
	chk := &seqChecker{}
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, RotateAfter: 250 * time.Millisecond,
		HandoverGrace: 150 * time.Millisecond, Budget: NewBudget(100000, time.Minute, nil)}, chk.handle)
	const want = 10
	deadline := time.Now().Add(20 * time.Second)
	for c.Stats().Rotations.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	chk.mu.Lock()
	defer chk.mu.Unlock()
	t.Logf("rotations=%d last=%d gaps=%d dups=%d retired_dropped=%d", c.Stats().Rotations.Load(), chk.last, chk.gaps, chk.dups,
		c.Stats().RetiredDropped.Load())
	if c.Stats().Rotations.Load() < want {
		t.Fatalf("only %d rotations", c.Stats().Rotations.Load())
	}
	if chk.gaps != 0 {
		t.Fatalf("%d events lost across %d rotations", chk.gaps, c.Stats().Rotations.Load())
	}
	if chk.dups == 0 {
		t.Fatal("expected duplicates from the overlap; the test did not exercise a handover")
	}
}

// TestOldConnectionEndsDuringGrace: the replacement is promoted as soon as
// the old connection ends, without waiting for the (long) grace, and the
// held messages are delivered.
func TestOldConnectionEndsDuringGrace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub(ctx, time.Millisecond)
	stopOld := make(chan struct{})
	var oldLast atomic.Int64
	fs := newFakeServer(t, func(rctx context.Context, idx int, c *websocket.Conn) {
		rctx = c.CloseRead(rctx)
		ch, unsub := h.subscribe()
		defer unsub()
		stop := stopOld
		if idx != 1 {
			stop = nil // only the first connection is told to stop
		}
		for {
			select {
			case <-rctx.Done():
				return
			case <-stop:
				return // the old connection ends while the new one is held
			case ev := <-ch:
				if send(rctx, c, fmt.Sprintf(aggTradeMsg, ev.id)) != nil {
					return
				}
				if idx == 1 {
					oldLast.Store(ev.id)
				}
			}
		}
	})
	chk := &seqChecker{}
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, RotateAfter: 4500 * time.Millisecond,
		HandoverGrace: 4 * time.Second}, chk.handle)
	deadline := time.Now().Add(10 * time.Second)
	for c.Stats().Handovers.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().Handovers.Load() < 1 {
		t.Fatal("handover never started")
	}
	close(stopOld)
	stopped := time.Now()
	for c.Stats().Rotations.Load() < 1 && time.Since(stopped) < time.Second {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().Rotations.Load() < 1 {
		t.Fatal("replacement not promoted within 1s of the old connection ending (grace 4s)")
	}
	time.Sleep(50 * time.Millisecond)
	chk.mu.Lock()
	defer chk.mu.Unlock()
	if chk.gaps != 0 || chk.last <= oldLast.Load() {
		t.Fatalf("gaps=%d last=%d, old connection's last=%d", chk.gaps, chk.last, oldLast.Load())
	}
}

// TestReplacementFailsDuringGrace: held messages are dropped, the old
// connection keeps delivering and the replacement is retried.
func TestReplacementFailsDuringGrace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHub(ctx, time.Millisecond)
	fs := newFakeServer(t, func(rctx context.Context, idx int, c *websocket.Conn) {
		rctx = c.CloseRead(rctx)
		ch, unsub := h.subscribe()
		defer unsub()
		n := 0
		for {
			select {
			case <-rctx.Done():
				return
			case ev := <-ch:
				if idx > 1 && n == 5 {
					return // every replacement dies after a few messages
				}
				n++
				if send(rctx, c, fmt.Sprintf(aggTradeMsg, ev.id)) != nil {
					return
				}
			}
		}
	})
	chk := &seqChecker{}
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, RotateAfter: 100 * time.Millisecond,
		HandoverGrace: 90 * time.Millisecond}, chk.handle)
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().RotationFailures.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	st := c.Stats()
	if st.RotationFailures.Load() < 3 || st.Rotations.Load() != 0 || st.Disconnects.Load() != 0 {
		t.Fatalf("failures=%d rotations=%d disconnects=%d", st.RotationFailures.Load(), st.Rotations.Load(), st.Disconnects.Load())
	}
	chk.mu.Lock()
	defer chk.mu.Unlock()
	if chk.gaps != 0 {
		t.Fatalf("the old connection's stream has %d gaps", chk.gaps)
	}
}
