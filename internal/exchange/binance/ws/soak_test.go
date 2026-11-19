package ws

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestActivityMeasuresFromBase(t *testing.T) {
	t.Parallel()
	base := time.Now() // carries a monotonic reading
	a := newActivity(base)
	if got := a.since(base.Add(2 * time.Second)); got != 2*time.Second {
		t.Fatalf("since before touch = %v, want 2s", got)
	}
	a.touch(base.Add(10 * time.Second))
	if got := a.since(base.Add(25 * time.Second)); got != 15*time.Second {
		t.Fatalf("since = %v, want 15s", got)
	}
	// Fake clocks without a monotonic reading behave the same way.
	fake := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	b := newActivity(fake)
	b.touch(fake.Add(time.Minute))
	if got := b.since(fake.Add(90 * time.Second)); got != 30*time.Second {
		t.Fatalf("fake since = %v, want 30s", got)
	}
}

// TestActivityTakesTheLargerClock: a resume after suspend shows up only on
// the wall clock, a backward wall-clock step only on the monotonic clock.
// Neither can be produced with real clocks in a test, so the stored readings
// are set directly.
func TestActivityTakesTheLargerClock(t *testing.T) {
	t.Parallel()
	base := time.Now()
	a := newActivity(base)
	a.lastWall.Store(base.Add(-time.Hour).UnixNano()) // as after a one-hour suspend
	if got := a.since(base); got < time.Hour {
		t.Fatalf("suspend: since = %v, want >= 1h", got)
	}
	b := newActivity(base)
	b.lastWall.Store(base.Add(time.Hour).UnixNano()) // wall clock stepped back by an hour
	b.lastMono.Store(int64(-time.Minute))            // but the connection really is a minute idle
	if got := b.since(base); got != time.Minute {
		t.Fatalf("backward step: since = %v, want 1m", got)
	}
}

func TestStoreMax(t *testing.T) {
	t.Parallel()
	var v atomic.Int64
	for _, x := range []int64{3, 1, 7, 7, 2} {
		storeMax(&v, x)
	}
	if v.Load() != 7 {
		t.Fatalf("max = %d", v.Load())
	}
}

func TestSlowHandlerIsVisibleInStats(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		for i := 1; i <= 50; i++ {
			if send(ctx, c, fmt.Sprintf(aggTradeMsg, i)) != nil {
				return
			}
		}
		ctx = c.CloseRead(ctx)
		<-ctx.Done()
	})
	var got atomic.Int64
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, func(Message) {
		time.Sleep(2 * time.Millisecond)
		got.Add(1)
	})
	deadline := time.Now().Add(5 * time.Second)
	for got.Load() < 50 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got.Load() < 50 {
		t.Fatalf("delivered %d of 50", got.Load())
	}
	if hw := c.Stats().QueueHighWater.Load(); hw < 2 {
		t.Fatalf("queue high water %d, want > 1 with a slow handler", hw)
	}
	if lag := time.Duration(c.Stats().MaxDeliveryLagNs.Load()); lag < 2*time.Millisecond {
		t.Fatalf("max delivery lag %v, want >= one handler call", lag)
	}
}

// TestSoakManyRotations runs a compressed day: many planned rotations,
// server shutdowns and abrupt drops. The goroutine count must not grow with
// the number of connections, and must return to the baseline after Run.
func TestSoakManyRotations(t *testing.T) {
	// Not parallel: counts process goroutines.
	if testing.Short() {
		t.Skip("soak test")
	}
	before := runtime.NumGoroutine()
	var nextID atomic.Int64
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		ctx = c.CloseRead(ctx)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for n := 0; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			switch {
			case idx%5 == 0 && n == 5:
				if send(ctx, c, shutdownMsg) != nil {
					return
				}
			case idx%7 == 0 && n == 8:
				return // abrupt drop
			default:
				if send(ctx, c, fmt.Sprintf(aggTradeMsg, nextID.Add(1))) != nil {
					return
				}
			}
		}
	})
	c, err := New(Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, RotateAfter: 30 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond, HandoverGrace: 5 * time.Millisecond,
		Budget: NewBudget(100000, time.Minute, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var msgs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx, func(Message) { msgs.Add(1) }); close(done) }()

	// minGoroutines samples for d and returns the smallest count, which
	// filters out connections that are being opened or torn down.
	minGoroutines := func(d time.Duration) int {
		m := runtime.NumGoroutine()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if n := runtime.NumGoroutine(); n < m {
				m = n
			}
			time.Sleep(2 * time.Millisecond)
		}
		return m
	}
	time.Sleep(500 * time.Millisecond)
	early := minGoroutines(500 * time.Millisecond)
	rotEarly := c.Stats().Rotations.Load()
	time.Sleep(1500 * time.Millisecond)
	late := minGoroutines(500 * time.Millisecond)
	rotLate := c.Stats().Rotations.Load()
	cancel()
	<-done
	fs.Close()

	st := c.Stats()
	t.Logf("dials=%d rotations=%d failures=%d disconnects=%d shutdowns=%d messages=%d goroutines early=%d late=%d",
		st.Dials.Load(), st.Rotations.Load(), st.RotationFailures.Load(), st.Disconnects.Load(),
		st.ShutdownEvents.Load(), msgs.Load(), early, late)
	if rotLate-rotEarly < 20 {
		t.Fatalf("only %d rotations in the measured window; the soak did not exercise rotation", rotLate-rotEarly)
	}
	if st.ShutdownEvents.Load() == 0 || msgs.Load() == 0 || st.Malformed.Load() != 0 {
		t.Fatalf("shutdowns=%d messages=%d malformed=%d", st.ShutdownEvents.Load(), msgs.Load(), st.Malformed.Load())
	}
	// A leak of one goroutine per connection would add dozens here; the
	// small allowance covers connections caught mid-handshake while sampling.
	if late > early+5 {
		t.Fatalf("goroutines grew from %d to %d over %d rotations", early, late, rotLate-rotEarly)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Fatalf("goroutines before %d, after %d", before, n)
	}
}
