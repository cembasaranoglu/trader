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
	"regexp"
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
	DefaultMaxMessage   = 256 << 10
	DefaultMinBackoff   = time.Second
	DefaultMaxBackoff   = 60 * time.Second
	DefaultDialTimeout  = 15 * time.Second
	documentedConnLimit = 300
	documentedConnWin   = 5 * time.Minute
)

// streamName allows a lower-case symbol and a stream kind such as
// "depth@100ms" or "kline_1M" (an allow-list, not a deny-list).
var streamName = regexp.MustCompile(`^[a-z0-9]{1,32}@[A-Za-z0-9_@]{1,32}$`)

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
	Dials            atomic.Int64
	DialErrors       atomic.Int64
	Disconnects      atomic.Int64
	Rotations        atomic.Int64 // completed replacements (planned or after serverShutdown)
	RotationFailures atomic.Int64 // replacement dials or connections that failed
	ShutdownEvents   atomic.Int64
	Messages         atomic.Int64
	Malformed        atomic.Int64
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
		if !streamName.MatchString(s) {
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
	cfg        Config
	url        string
	hc         *http.Client
	log        *slog.Logger
	now        func() time.Time
	budget     *Budget
	nextID     atomic.Uint64
	stats      Stats
	subscribed map[string]bool
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
	subscribed := make(map[string]bool, len(cfg.Streams))
	for _, st := range cfg.Streams {
		subscribed[st] = true
	}
	return &Client{cfg: cfg, url: u, hc: httpx.NoRedirectClient(cfg.HTTPClient, 0), log: cfg.Logger,
		now: cfg.Now, budget: cfg.Budget, subscribed: subscribed}, nil
}

// Stats returns the client's counters.
func (c *Client) Stats() *Stats { return &c.stats }

type connEvent struct {
	id  uint64
	msg *Message
	err error // non-nil: the connection ended
}

// live is one open connection. Its reader starts only after the event loop
// has registered it, so no event can arrive for an unknown connection.
type live struct {
	id     uint64
	opened time.Time
	cancel context.CancelFunc
	conn   *websocket.Conn
	act    *activity
	ctx    context.Context
}

type dialResult struct {
	l       *live
	err     error
	replace bool
}

// healthyAfter is how long a connection must have lived before the
// reconnect backoff resets; a server that accepts and drops connections
// immediately therefore cannot drive a tight reconnect loop.
const healthyAfter = time.Minute

// Run connects and delivers messages to h until ctx is done; it returns
// ctx.Err(). Dials run in the background so the event loop keeps reading
// (and answering pings) while a replacement connects. Initial connections
// and failed replacements are retried with jittered exponential backoff.
// Malformed messages close the connection.
func (c *Client) Run(ctx context.Context, h Handler) error {
	events := make(chan connEvent, 1024)
	dials := make(chan dialResult, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var cur, next *live // next: a replacement that has not delivered a message yet
	dialing := false
	attempt, replAttempt := 0, 0

	startDial := func(replace bool) {
		dialing = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := c.dial(ctx)
			select {
			case dials <- dialResult{l: l, err: err, replace: replace}:
			case <-ctx.Done():
				if l != nil {
					l.cancel()
					l.conn.CloseNow()
				}
			}
		}()
	}

	newTimer := func() *time.Timer {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	}
	rotate, retry, replRetry := newTimer(), newTimer(), newTimer()
	defer rotate.Stop()
	defer retry.Stop()
	defer replRetry.Stop()
	armRotate := func(l *live) {
		d := c.cfg.RotateAfter - c.now().Sub(l.opened)
		if d < 0 {
			d = 0
		}
		rotate.Reset(d)
	}
	startReader := func(l *live) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.readLoop(l.ctx, &wg, l.id, l.conn, l.act, events)
		}()
	}
	scheduleReplacement := func() {
		replRetry.Reset(c.backoff(replAttempt))
		replAttempt++
	}

	startDial(false)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-retry.C:
			if cur == nil && !dialing {
				startDial(false)
			}

		case <-rotate.C:
			if cur != nil && next == nil && !dialing {
				startDial(true)
			}

		case <-replRetry.C:
			if cur != nil && next == nil && !dialing {
				startDial(true)
			}

		case r := <-dials:
			dialing = false
			if r.err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.log.Warn("ws dial failed", "replacement", r.replace, "err", r.err)
				switch {
				case cur == nil:
					retry.Reset(c.backoff(attempt))
					attempt++
				case r.replace:
					c.stats.RotationFailures.Add(1)
					scheduleReplacement()
				}
				continue
			}
			switch {
			case cur == nil:
				cur = r.l
				startReader(cur)
				armRotate(cur)
			case next == nil:
				next = r.l
				startReader(next)
			default:
				r.l.cancel() // a replacement is already pending
				r.l.conn.CloseNow()
			}

		case ev := <-events:
			if ev.err != nil {
				switch {
				case next != nil && ev.id == next.id:
					// The replacement failed before delivering; retry it.
					next.cancel()
					next = nil
					c.stats.RotationFailures.Add(1)
					scheduleReplacement()
				case cur != nil && ev.id == cur.id:
					c.stats.Disconnects.Add(1)
					if c.now().Sub(cur.opened) >= healthyAfter {
						attempt = 0
					}
					cur.cancel()
					if next != nil {
						cur, next = next, nil // promote the replacement
						c.stats.Rotations.Add(1)
						replAttempt = 0
						armRotate(cur)
						continue
					}
					cur = nil
					if !errors.Is(ev.err, context.Canceled) {
						c.log.Warn("ws connection ended", "err", ev.err)
					}
					if !dialing {
						retry.Reset(c.backoff(attempt))
						attempt++
					}
				}
				continue
			}
			if next != nil && ev.id == next.id && cur != nil {
				// The replacement is delivering: retire the old connection.
				cur.cancel()
				cur, next = next, nil
				c.stats.Rotations.Add(1)
				replAttempt = 0
				armRotate(cur)
			}
			if _, ok := ev.msg.Event.(*ServerShutdownEvent); ok {
				c.stats.ShutdownEvents.Add(1)
				if cur != nil && ev.id == cur.id && next == nil && !dialing {
					replRetry.Stop()
					startDial(true)
				}
			}
			c.stats.Messages.Add(1)
			h(*ev.msg)
		}
	}
}

// dial waits for the connection budget and connects. The caller starts the reader.
func (c *Client) dial(ctx context.Context) (*live, error) {
	if err := c.budget.Wait(ctx); err != nil {
		return nil, err
	}
	act := &activity{}
	act.touch(c.now())
	dctx, dcancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	conn, _, err := websocket.Dial(dctx, c.url, &websocket.DialOptions{HTTPClient: c.hc,
		OnPingReceived: func(context.Context, []byte) bool { act.touch(c.now()); return true }})
	dcancel()
	c.stats.Dials.Add(1)
	if err != nil {
		c.stats.DialErrors.Add(1)
		return nil, err
	}
	conn.SetReadLimit(c.cfg.MaxMessage)
	cctx, ccancel := context.WithCancel(ctx)
	return &live{id: c.nextID.Add(1), opened: c.now(), cancel: ccancel, conn: conn, act: act, ctx: cctx}, nil
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

func (c *Client) readLoop(ctx context.Context, wg *sync.WaitGroup, id uint64, conn *websocket.Conn, act *activity, out chan<- connEvent) {
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
	wg.Add(1)
	go func() {
		defer wg.Done()
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
		if err == nil && stream != "!serverShutdown" && !c.subscribed[stream] {
			err = fmt.Errorf("%w: message for unsubscribed stream %q", ErrMalformed, clip(stream))
		}
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
