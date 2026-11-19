// Package ws is a client for Binance spot combined market-data streams.
//
// Facts it relies on (binance-spot-api-docs, web-socket-streams.md): combined
// streams at /stream?streams=a/b/c wrap events as {"stream":...,"data":...};
// symbols are lowercase; a connection lasts at most 24 hours; the server pings
// every 20 seconds and drops the connection if no pong arrives within a
// minute (the websocket library answers pings with the same payload); a
// serverShutdown event announces a disconnection; at most 1024 streams per
// connection and 300 connection attempts per 5 minutes per IP.
//
// Run keeps one connection alive and replaces it before the 24-hour limit or
// when the server announces a shutdown. During a replacement the old and the
// new connection overlap briefly, so consumers must tolerate duplicates
// (order book update IDs, kline open times, aggregate trade IDs).
package ws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/cembasaranoglu/trader/internal/exchange/httpx"
)

// Defaults and documented limits.
const (
	DefaultURL          = "wss://data-stream.binance.vision"
	MaxStreams          = 1024
	DefaultReadTimeout  = 60 * time.Second
	DefaultRotateAfter  = 23*time.Hour + 50*time.Minute
	DefaultMaxMessage   = 1 << 20
	DefaultMinBackoff   = time.Second
	DefaultMaxBackoff   = 60 * time.Second
	DefaultDialTimeout  = 15 * time.Second
	documentedConnLimit = 300
	documentedConnWin   = 5 * time.Minute
)

// ErrNoStreams is returned when Config.Streams is empty.
var ErrNoStreams = errors.New("ws: no streams")

// Config configures Run. Zero values select the defaults above.
type Config struct {
	URL         string
	Streams     []string
	HTTPClient  *http.Client // copied; never follows redirects
	ReadTimeout time.Duration
	RotateAfter time.Duration
	MaxMessage  int64
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	DialTimeout time.Duration
	// Budget limits connection attempts; nil selects a budget of half the
	// documented 300 attempts per 5 minutes. Share one Budget between all
	// connections from the same IP.
	Budget *Budget
	Logger *slog.Logger
	Now    func() time.Time
}

// Stats are cumulative counters, safe to read concurrently.
type Stats struct {
	Dials          atomic.Int64
	DialErrors     atomic.Int64
	Disconnects    atomic.Int64
	Rotations      atomic.Int64
	ShutdownEvents atomic.Int64
	Messages       atomic.Int64
	Malformed      atomic.Int64
}

// Handler receives decoded messages from a single goroutine, in arrival order.
type Handler func(Message)

// StreamURL builds the combined-stream URL and validates the stream names.
func StreamURL(base string, streams []string) (string, error) {
	if len(streams) == 0 {
		return "", ErrNoStreams
	}
	if len(streams) > MaxStreams {
		return "", fmt.Errorf("ws: %d streams exceed the documented maximum of %d per connection", len(streams), MaxStreams)
	}
	if base == "" {
		base = DefaultURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("ws: invalid URL %q", base)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !httpx.IsLoopback(u.Hostname()) {
			return "", fmt.Errorf("ws: URL %q: wss required for non-loopback hosts", base)
		}
	default:
		return "", fmt.Errorf("ws: URL %q: unsupported scheme", base)
	}
	seen := make(map[string]bool, len(streams))
	for _, s := range streams {
		sym, kind, ok := strings.Cut(s, "@")
		if !ok || sym == "" || kind == "" || sym != strings.ToLower(sym) || strings.ContainsAny(s, "/?&#% ") {
			return "", fmt.Errorf("ws: invalid stream name %q", s)
		}
		if seen[s] {
			return "", fmt.Errorf("ws: duplicate stream %q", s)
		}
		seen[s] = true
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/stream"
	u.RawQuery = "streams=" + strings.Join(streams, "/")
	return u.String(), nil
}

// Client runs one combined-stream subscription.
type Client struct {
	cfg    Config
	url    string
	hc     *http.Client
	log    *slog.Logger
	now    func() time.Time
	budget *Budget
	nextID atomic.Uint64
	stats  Stats
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := StreamURL(cfg.URL, cfg.Streams)
	if err != nil {
		return nil, err
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&cfg.ReadTimeout, DefaultReadTimeout)
	def(&cfg.RotateAfter, DefaultRotateAfter)
	def(&cfg.MinBackoff, DefaultMinBackoff)
	def(&cfg.MaxBackoff, DefaultMaxBackoff)
	def(&cfg.DialTimeout, DefaultDialTimeout)
	if cfg.MaxMessage == 0 {
		cfg.MaxMessage = DefaultMaxMessage
	}
	switch {
	case cfg.ReadTimeout < 0, cfg.RotateAfter < 0, cfg.MinBackoff < 0, cfg.DialTimeout < 0, cfg.MaxMessage < 0:
		return nil, errors.New("ws: negative durations or sizes are not allowed")
	case cfg.MaxBackoff < cfg.MinBackoff:
		return nil, errors.New("ws: MaxBackoff must be >= MinBackoff")
	case cfg.RotateAfter >= 24*time.Hour:
		return nil, errors.New("ws: RotateAfter must be below the documented 24h connection limit")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Budget == nil {
		cfg.Budget = NewBudget(documentedConnLimit/2, documentedConnWin, cfg.Now)
	}
	return &Client{cfg: cfg, url: u, hc: httpx.NoRedirectClient(cfg.HTTPClient, 0), log: cfg.Logger,
		now: cfg.Now, budget: cfg.Budget}, nil
}

// Stats returns the client's counters.
func (c *Client) Stats() *Stats { return &c.stats }

type connEvent struct {
	id  uint64
	msg *Message
	err error // non-nil: the connection ended
}

// Run connects and delivers messages to h until ctx is done. It returns
// ctx.Err(). Connection failures are retried with jittered exponential
// backoff; malformed messages close the connection and are retried too.
func (c *Client) Run(ctx context.Context, h Handler) error {
	events := make(chan connEvent, 4096)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type live struct {
		id     uint64
		cancel context.CancelFunc
	}
	var cur, next *live // next is a replacement that has not delivered a message yet
	attempt := 0

	start := func() (*live, error) {
		if err := c.budget.Wait(ctx); err != nil {
			return nil, err
		}
		dctx, dcancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
		act := &activity{}
		act.touch(c.now())
		conn, _, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{HTTPClient: c.hc,
			OnPingReceived: func(context.Context, []byte) bool { act.touch(c.now()); return true }})
		dcancel()
		c.stats.Dials.Add(1)
		if err != nil {
			c.stats.DialErrors.Add(1)
			return nil, err
		}
		conn.SetReadLimit(c.cfg.MaxMessage)
		id := c.nextID.Add(1)
		cctx, ccancel := context.WithCancel(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.readLoop(cctx, id, conn, act, events)
		}()
		return &live{id: id, cancel: ccancel}, nil
	}

	rotate := time.NewTimer(time.Hour)
	rotate.Stop()
	defer rotate.Stop()
	armRotate := func() {
		rotate.Stop()
		rotate.Reset(c.cfg.RotateAfter)
	}

	for {
		if cur == nil {
			l, err := start()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.log.Warn("ws dial failed", "attempt", attempt, "err", err)
				if err := sleep(ctx, c.backoff(attempt)); err != nil {
					return err
				}
				attempt++
				continue
			}
			cur = l
			armRotate()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rotate.C:
			if next == nil {
				c.stats.Rotations.Add(1)
				if l, err := start(); err == nil {
					next = l
				} else {
					c.log.Warn("ws planned rotation dial failed; keeping current connection", "err", err)
					rotate.Reset(time.Minute)
				}
			}
		case ev := <-events:
			if ev.err != nil {
				switch {
				case next != nil && ev.id == next.id:
					// The replacement failed before delivering; retry later.
					next.cancel()
					next = nil
					rotate.Reset(time.Minute)
				case cur != nil && ev.id == cur.id:
					c.stats.Disconnects.Add(1)
					cur.cancel()
					if next != nil {
						cur, next = next, nil // promote the replacement
						armRotate()
					} else {
						cur = nil
						if !errors.Is(ev.err, context.Canceled) {
							c.log.Warn("ws connection ended", "err", ev.err)
						}
						if err := sleep(ctx, c.backoff(attempt)); err != nil {
							return err
						}
						attempt++
					}
				}
				continue
			}
			attempt = 0
			if next != nil && ev.id == next.id && cur != nil {
				// The replacement is delivering: retire the old connection.
				cur.cancel()
				cur, next = next, nil
				armRotate()
			}
			if _, ok := ev.msg.Event.(*ServerShutdownEvent); ok {
				c.stats.ShutdownEvents.Add(1)
				if next == nil && cur != nil && ev.id == cur.id {
					if l, err := start(); err == nil {
						next = l
					} else {
						c.log.Warn("ws replacement after serverShutdown failed", "err", err)
					}
				}
			}
			c.stats.Messages.Add(1)
			h(*ev.msg)
		}
	}
}

// activity records the last time a connection showed life: a data message
// or a server ping. The server pings every 20 seconds, so a quiet stream
// (no market changes) is not mistaken for a dead connection.
type activity struct{ last atomic.Int64 }

func (a *activity) touch(t time.Time) { a.last.Store(t.UnixNano()) }
func (a *activity) since(t time.Time) time.Duration {
	return t.Sub(time.Unix(0, a.last.Load()))
}

// errIdle reports a connection with no data and no ping for ReadTimeout.
var errIdle = errors.New("ws: no data or ping within the read timeout")

func (c *Client) readLoop(ctx context.Context, id uint64, conn *websocket.Conn, act *activity, out chan<- connEvent) {
	defer conn.CloseNow()
	send := func(ev connEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Watchdog: close the connection when it has been idle for ReadTimeout.
	go func() {
		tick := c.cfg.ReadTimeout / 4
		if tick < 10*time.Millisecond {
			tick = 10 * time.Millisecond
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				if act.since(c.now()) > c.cfg.ReadTimeout {
					cancel(errIdle)
					return
				}
			}
		}
	}()
	for {
		typ, b, err := conn.Read(rctx)
		if err != nil {
			if cause := context.Cause(rctx); cause != nil {
				err = cause
			}
			send(connEvent{id: id, err: err})
			return
		}
		act.touch(c.now())
		if typ != websocket.MessageText {
			c.stats.Malformed.Add(1)
			send(connEvent{id: id, err: fmt.Errorf("%w: non-text frame", ErrMalformed)})
			return
		}
		stream, ev, err := Decode(b)
		if err != nil {
			c.stats.Malformed.Add(1)
			send(connEvent{id: id, err: err})
			return
		}
		if !send(connEvent{id: id, msg: &Message{Stream: stream, Event: ev, ConnID: id, Received: c.now()}}) {
			return
		}
	}
}

// backoff returns a jittered exponential delay in [d/2, d] where
// d = min(MaxBackoff, MinBackoff * 2^attempt).
func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.MinBackoff
	for i := 0; i < attempt && d < c.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > c.cfg.MaxBackoff {
		d = c.cfg.MaxBackoff
	}
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Budget limits connection attempts to max per sliding window. It is safe
// for concurrent use.
type Budget struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	times  []time.Time
}

// NewBudget returns a Budget. now may be nil (time.Now).
func NewBudget(max int, window time.Duration, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	if max < 1 {
		max = 1
	}
	return &Budget{max: max, window: window, now: now}
}

// Reserve records an attempt if one is available and returns 0, or returns
// how long to wait before an attempt becomes available.
func (b *Budget) Reserve() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	cut := 0
	for cut < len(b.times) && now.Sub(b.times[cut]) >= b.window {
		cut++
	}
	b.times = b.times[cut:]
	if len(b.times) < b.max {
		b.times = append(b.times, now)
		return 0
	}
	return b.window - now.Sub(b.times[0])
}

// Wait blocks until an attempt is available or ctx is done.
func (b *Budget) Wait(ctx context.Context) error {
	for {
		d := b.Reserve()
		if d <= 0 {
			return nil
		}
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}
