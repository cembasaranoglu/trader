package ws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	aggTradeMsg = `{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1700000000000,"s":"BTCUSDT","a":%d,"p":"90500.01","q":"0.001","f":1,"l":2,"T":1700000000000,"m":true,"M":true}}`
	shutdownMsg = `{"stream":"!serverShutdown","data":{"e":"serverShutdown","E":1700000000000,"T":1700000600000}}`
)

// fakeServer accepts WebSocket connections; behave runs once per connection
// with its 1-based index.
type fakeServer struct {
	*httptest.Server
	conns atomic.Int32
	paths sync.Map
}

func newFakeServer(t *testing.T, behave func(ctx context.Context, idx int, c *websocket.Conn)) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(fs.conns.Add(1))
		fs.paths.Store(idx, r.URL.RequestURI())
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		behave(r.Context(), idx, c)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) wsURL() string { return "ws" + strings.TrimPrefix(fs.URL, "http") }

func send(ctx context.Context, c *websocket.Conn, s string) error {
	return c.Write(ctx, websocket.MessageText, []byte(s))
}

// collector gathers messages delivered to the handler.
type collector struct {
	mu   sync.Mutex
	msgs []Message
	ch   chan Message
}

func newCollector() *collector { return &collector{ch: make(chan Message, 1024)} }

func (c *collector) handle(m Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	c.ch <- m
}

func (c *collector) wait(t *testing.T, pred func(Message) bool) Message {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.ch:
			if pred(m) {
				return m
			}
		case <-timeout:
			t.Fatal("timed out waiting for message")
		}
	}
}

func runClient(t *testing.T, cfg Config, h Handler) (*Client, context.CancelFunc, <-chan error) {
	t.Helper()
	cfg.MinBackoff, cfg.MaxBackoff = 10*time.Millisecond, 20*time.Millisecond
	if cfg.HandoverGrace == 0 {
		cfg.HandoverGrace = 20 * time.Millisecond
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1) // receives Run's result once
	finished := make(chan struct{})
	go func() { done <- c.Run(ctx, h); close(finished) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return c, cancel, done
}

func TestDeliversAndAnswersPing(t *testing.T) {
	t.Parallel()
	pinged := make(chan error, 1)
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, 1))
		go func() {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			pinged <- c.Ping(pctx) // succeeds only if the client answers with a pong
		}()
		rctx := c.CloseRead(ctx)
		<-rctx.Done()
	})
	col := newCollector()
	runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, col.handle)
	m := col.wait(t, func(Message) bool { return true })
	ev, ok := m.Event.(*AggTradeEvent)
	if !ok || ev.AggID != 1 || ev.Price != "90500.01" || m.Stream != "btcusdt@aggTrade" {
		t.Fatalf("unexpected message %+v", m)
	}
	if path, _ := fs.paths.Load(1); path != "/stream?streams=btcusdt@aggTrade" {
		t.Fatalf("path %v", path)
	}
	select {
	case err := <-pinged:
		if err != nil {
			t.Fatalf("ping not answered: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ping timed out")
	}
}

func TestSilentConnectionIsReplaced(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		if idx >= 2 {
			_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, idx))
		}
		<-ctx.Done() // first connection stays silent
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, ReadTimeout: 150 * time.Millisecond}, col.handle)
	col.wait(t, func(m Message) bool { return m.Event.(*AggTradeEvent).AggID == 2 })
	if c.Stats().Disconnects.Load() < 1 {
		t.Fatal("silent connection was not counted as a disconnect")
	}
}

func TestServerShutdownTriggersSeamlessReplacement(t *testing.T) {
	t.Parallel()
	firstClosed := make(chan struct{})
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		switch idx {
		case 1:
			_ = send(ctx, c, shutdownMsg)
			// Keep sending until the client retires this connection.
			for i := 100; ; i++ {
				if err := send(ctx, c, fmt.Sprintf(aggTradeMsg, i)); err != nil {
					close(firstClosed)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		default:
			for i := 1000; ctx.Err() == nil; i++ {
				_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, i))
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, col.handle)
	col.wait(t, func(m Message) bool { ev, ok := m.Event.(*AggTradeEvent); return ok && ev.AggID >= 1000 })
	select {
	case <-firstClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("old connection was not retired after the replacement delivered")
	}
	if c.Stats().ShutdownEvents.Load() != 1 || fs.conns.Load() != 2 {
		t.Fatalf("shutdown events %d, connections %d", c.Stats().ShutdownEvents.Load(), fs.conns.Load())
	}
}

func TestPlannedRotation(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		for i := idx * 1000; ctx.Err() == nil; i++ {
			if send(ctx, c, fmt.Sprintf(aggTradeMsg, i)) != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, RotateAfter: 200 * time.Millisecond}, col.handle)
	col.wait(t, func(m Message) bool { return m.Event.(*AggTradeEvent).AggID >= 2000 })
	if c.Stats().Rotations.Load() < 1 {
		t.Fatal("no rotation recorded")
	}
}

func TestMalformedAndOversizedMessagesReconnect(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"not json":       "{",
		"null data":      `{"stream":"btcusdt@aggTrade","data":null}`,
		"missing field":  `{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1,"s":"BTCUSDT","p":"1","q":"1","f":1,"l":1,"T":1,"m":true}}`,
		"unknown stream": `{"stream":"btcusdt@trade","data":{}}`,
		"oversized":      `{"stream":"btcusdt@aggTrade","data":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
				if idx == 1 {
					_ = send(ctx, c, bad)
				} else {
					_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, 7))
				}
				ctx = c.CloseRead(ctx)
				<-ctx.Done()
			})
			col := newCollector()
			c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, MaxMessage: 1024}, col.handle)
			col.wait(t, func(m Message) bool { return m.Event.(*AggTradeEvent).AggID == 7 })
			if name != "oversized" && c.Stats().Malformed.Load() != 1 {
				t.Fatalf("malformed counter %d", c.Stats().Malformed.Load())
			}
			if fs.conns.Load() < 2 {
				t.Fatal("expected a reconnect")
			}
		})
	}
}

func TestDialRedirectRefused(t *testing.T) {
	t.Parallel()
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redir.Close)
	c, _, _ := runClient(t, Config{URL: "ws" + strings.TrimPrefix(redir.URL, "http"), Streams: []string{"btcusdt@aggTrade"}}, func(Message) {})
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().DialErrors.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Stats().DialErrors.Load() < 2 || targetHits.Load() != 0 {
		t.Fatalf("dial errors %d, redirect target hits %d", c.Stats().DialErrors.Load(), targetHits.Load())
	}
}

func TestRunReturnsOnCancel(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		ctx = c.CloseRead(ctx)
		<-ctx.Done()
	})
	_, cancel, done := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, func(Message) {})
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestStreamURLValidation(t *testing.T) {
	t.Parallel()
	good, err := StreamURL("", []string{"btcusdt@aggTrade", "btcusdt@kline_1M", "ethusdt@depth@100ms"})
	if err != nil || good != "wss://data-stream.binance.vision/stream?streams=btcusdt@aggTrade/btcusdt@kline_1M/ethusdt@depth@100ms" {
		t.Fatalf("%q %v", good, err)
	}
	many := make([]string, MaxStreams+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d@aggTrade", i)
	}
	for name, tc := range map[string]struct {
		base    string
		streams []string
	}{
		"empty":         {"", nil},
		"too many":      {"", many},
		"uppercase sym": {"", []string{"BTCUSDT@aggTrade"}},
		"no kind":       {"", []string{"btcusdt"}},
		"injection":     {"", []string{"btcusdt@aggTrade/ethusdt@trade"}},
		"duplicate":     {"", []string{"a@aggTrade", "a@aggTrade"}},
		"plain ws":      {"ws://data-stream.binance.vision", []string{"a@aggTrade"}},
		"https scheme":  {"https://data-stream.binance.vision", []string{"a@aggTrade"}},
		"query in base": {"wss://x.example?y=1", []string{"a@aggTrade"}},
	} {
		if _, err := StreamURL(tc.base, tc.streams); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := StreamURL("ws://127.0.0.1:1", []string{"a@aggTrade"}); err != nil {
		t.Errorf("loopback ws must be allowed: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"rotate at 24h":     {Streams: []string{"a@aggTrade"}, RotateAfter: 24 * time.Hour},
		"grace >= rotate":   {Streams: []string{"a@aggTrade"}, RotateAfter: time.Second, HandoverGrace: time.Second},
		"grace too long":    {Streams: []string{"a@aggTrade"}, HandoverGrace: MaxHandover + 1},
		"handover past 24h": {Streams: []string{"a@aggTrade"}, RotateAfter: 24*time.Hour - 10*time.Second},
		"negative grace":    {Streams: []string{"a@aggTrade"}, HandoverGrace: -1},
		"backoff inverted":  {Streams: []string{"a@aggTrade"}, MinBackoff: time.Minute, MaxBackoff: time.Second},
		"negative timeout":  {Streams: []string{"a@aggTrade"}, ReadTimeout: -1},
		"negative max size": {Streams: []string{"a@aggTrade"}, MaxMessage: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBackoffBounds(t *testing.T) {
	t.Parallel()
	c, err := New(Config{Streams: []string{"a@aggTrade"}, MinBackoff: time.Second, MaxBackoff: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		want := time.Second << min(attempt, 6)
		if want > 60*time.Second {
			want = 60 * time.Second
		}
		for i := 0; i < 50; i++ {
			d := c.backoff(attempt)
			if d < want/2 || d > want {
				t.Fatalf("attempt %d: %s not in [%s, %s]", attempt, d, want/2, want)
			}
		}
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	b := NewBudget(3, 5*time.Minute, clock)
	for i := 0; i < 3; i++ {
		if d := b.Reserve(); d != 0 {
			t.Fatalf("reserve %d: wait %s", i, d)
		}
	}
	if d := b.Reserve(); d != 5*time.Minute {
		t.Fatalf("4th reserve must wait the full window, got %s", d)
	}
	mu.Lock()
	now = now.Add(5 * time.Minute)
	mu.Unlock()
	if d := b.Reserve(); d != 0 {
		t.Fatalf("after window: wait %s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b2 := NewBudget(1, time.Hour, clock)
	b2.Reserve()
	if err := b2.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait must honour cancellation: %v", err)
	}
}

// A quiet stream that is still pinged by the server must not be replaced.
func TestPingsKeepQuietConnectionAlive(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		ctx = c.CloseRead(ctx) // needed for the server's Ping to read pongs
		for ctx.Err() == nil {
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			_ = c.Ping(pctx)
			cancel()
			time.Sleep(40 * time.Millisecond)
		}
	})
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, ReadTimeout: 200 * time.Millisecond}, func(Message) {})
	time.Sleep(800 * time.Millisecond)
	if d := c.Stats().Disconnects.Load(); d != 0 || fs.conns.Load() != 1 {
		t.Fatalf("pinged connection was replaced: disconnects %d, connections %d", d, fs.conns.Load())
	}
}

// While the replacement waits for the connection budget, the current
// connection must keep delivering (the event loop must not block).
func TestReplacementDoesNotBlockDelivery(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		_ = send(ctx, c, shutdownMsg)
		for i := 1; ctx.Err() == nil; i++ {
			if send(ctx, c, fmt.Sprintf(aggTradeMsg, i)) != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	col := newCollector()
	budget := NewBudget(1, time.Hour, nil) // the replacement can never get a slot
	runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, Budget: budget}, col.handle)
	col.wait(t, func(m Message) bool { ev, ok := m.Event.(*AggTradeEvent); return ok && ev.AggID >= 50 })
}

// A failed replacement after serverShutdown is retried with backoff.
func TestFailedReplacementIsRetried(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		switch idx {
		case 1:
			_ = send(ctx, c, shutdownMsg)
			for i := 1; ctx.Err() == nil; i++ {
				if send(ctx, c, fmt.Sprintf(aggTradeMsg, i)) != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		case 2:
			c.Close(websocket.StatusInternalError, "replacement fails")
		default:
			for i := 5000; ctx.Err() == nil; i++ {
				_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, i))
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, col.handle)
	col.wait(t, func(m Message) bool { ev, ok := m.Event.(*AggTradeEvent); return ok && ev.AggID >= 5000 })
	if c.Stats().RotationFailures.Load() < 1 || c.Stats().Rotations.Load() != 1 {
		t.Fatalf("failures %d rotations %d", c.Stats().RotationFailures.Load(), c.Stats().Rotations.Load())
	}
}

func TestConsecutiveShutdowns(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		if idx <= 2 {
			_ = send(ctx, c, shutdownMsg)
		}
		for i := idx * 1000; ctx.Err() == nil; i++ {
			if send(ctx, c, fmt.Sprintf(aggTradeMsg, i)) != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, col.handle)
	col.wait(t, func(m Message) bool { ev, ok := m.Event.(*AggTradeEvent); return ok && ev.AggID >= 3000 })
	if c.Stats().Rotations.Load() != 2 {
		t.Fatalf("rotations %d", c.Stats().Rotations.Load())
	}
}

func TestUnsubscribedStreamRejected(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		if idx == 1 {
			_ = send(ctx, c, strings.Replace(fmt.Sprintf(aggTradeMsg, 1), "btcusdt@aggTrade", "ethusdt@aggTrade", 1))
		} else {
			_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, 2))
		}
		ctx = c.CloseRead(ctx)
		<-ctx.Done()
	})
	col := newCollector()
	c, _, _ := runClient(t, Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}}, col.handle)
	col.wait(t, func(m Message) bool { return m.Event.(*AggTradeEvent).AggID == 2 })
	if c.Stats().Malformed.Load() != 1 {
		t.Fatalf("malformed %d", c.Stats().Malformed.Load())
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	// Not parallel: counts process goroutines.
	before := runtime.NumGoroutine()
	fs := newFakeServer(t, func(ctx context.Context, idx int, c *websocket.Conn) {
		_ = send(ctx, c, fmt.Sprintf(aggTradeMsg, 1))
		ctx = c.CloseRead(ctx)
		<-ctx.Done()
	})
	col := newCollector()
	c, err := New(Config{URL: fs.wsURL(), Streams: []string{"btcusdt@aggTrade"}, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, col.handle); close(done) }()
	col.wait(t, func(Message) bool { return true })
	cancel()
	<-done
	fs.Close()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Fatalf("goroutines before %d, after %d", before, n)
	}
}
