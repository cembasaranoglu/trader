package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cembasaranoglu/trader/internal/config"
	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
	"github.com/cembasaranoglu/trader/internal/marketdata"
)

// observe limits.
const (
	maxObserveSymbols  = 50
	maxObserveDuration = 7 * 24 * time.Hour
	minRotateAfter     = time.Minute
	// clockJumpThreshold is the difference between wall-clock and monotonic
	// elapsed time, within one sampling tick, that counts as a clock jump
	// (an NTP step, a manual change, or a suspend on systems whose monotonic
	// clock stops while suspended, such as Linux).
	clockJumpThreshold = 2 * time.Second
)

// observeReport is one JSON line of the observe output.
type observeReport struct {
	Type        string                  `json:"type"` // "minute" or "final"
	Time        time.Time               `json:"time"`
	ElapsedSec  int64                   `json:"elapsed_sec"`
	Interrupted bool                    `json:"interrupted,omitempty"`
	WS          wsReport                `json:"ws"`
	Runtime     runtimeReport           `json:"runtime"`
	Clock       clockReport             `json:"clock"`
	Symbols     map[string]symbolReport `json:"symbols"`
	RateLimit   map[string]string       `json:"rate_limit_headers"`
	Errors      []string                `json:"errors"`
}

type wsReport struct {
	Dials          int64 `json:"dials"`
	DialErrors     int64 `json:"dial_errors"`
	Disconnects    int64 `json:"disconnects"`
	Rotations      int64 `json:"rotations"`
	ShutdownEvents int64 `json:"server_shutdown_events"`
	Messages       int64 `json:"messages"`
	Malformed      int64 `json:"malformed"`
	RotationFails  int64 `json:"rotation_failures"`
	Handovers      int64 `json:"handovers"`
	RetiredDropped int64 `json:"retired_dropped"`
	QueueHighWater int64 `json:"queue_high_water"`
	MaxLagMs       int64 `json:"max_delivery_lag_ms"`
}

// runtimeReport shows whether the process grows over a long run.
type runtimeReport struct {
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	SysBytes       uint64 `json:"sys_bytes"`
	NumGC          uint32 `json:"num_gc"`
	GCPauseTotalMs int64  `json:"gc_pause_total_ms"`
}

// clockReport counts wall-clock jumps seen while observing (NTP steps,
// manual changes, and on Linux and macOS a suspend, during which the
// monotonic clock stops). Freshness and connection idleness are measured on
// both clocks, the larger age counting, so neither kind of jump makes old
// data look fresh.
type clockReport struct {
	Jumps       int64 `json:"jumps"`
	JumpSeconds int64 `json:"jump_seconds"`
}

type symbolReport struct {
	BookState      string `json:"book_state"`
	BookUpdateID   int64  `json:"book_update_id"`
	BookSnapshots  int64  `json:"book_snapshots"`
	BookGaps       int64  `json:"book_gaps"`
	BookCrossed    int64  `json:"book_crossed"`
	BookScaleUp    int64  `json:"book_scale_raises"`
	BookBadValues  int64  `json:"book_bad_values"`
	BookIgnored    int64  `json:"book_ignored_events"`
	SnapshotErrors int64  `json:"snapshot_errors"`
	Bars           int64  `json:"closed_bars"`
	BarsFromREST   int64  `json:"bars_filled_from_rest"`
	BarGapsUnfill  int64  `json:"bar_gaps_unfilled"`
	Trades         int64  `json:"trades"`
	TradeGaps      int64  `json:"trade_id_gaps"`
	Tickers        int64  `json:"book_tickers"`
	StaleSeconds   int64  `json:"stale_seconds"`
	LastEventAgeMs int64  `json:"last_event_age_ms"`
}

type discardSink struct{}

func (discardSink) OnBar(marketdata.Bar)       {}
func (discardSink) OnTrade(marketdata.Trade)   {}
func (discardSink) OnTicker(marketdata.Ticker) {}

func runObserve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the YAML configuration file (required)")
	symbols := fs.String("symbols", "", "comma-separated symbols, e.g. BTCUSDT,ETHUSDT (required)")
	duration := fs.Duration("duration", 24*time.Hour, "how long to observe (1s..168h)")
	rotateAfter := fs.Duration("rotate-after", 0, "replace the WebSocket connection after this long (1m..23h50m; default 23h50m)")
	out := fs.String("out", "", "write JSON lines to this file instead of stdout")
	baseURL := fs.String("base-url", "", "override the REST signed host (testing)")
	marketURL := fs.String("market-url", "", "override the REST market-data host (testing)")
	wsURL := fs.String("ws-url", "", "override the WebSocket market-data host (testing)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	names := splitSymbols(*symbols)
	if *path == "" || fs.NArg() != 0 || len(names) == 0 || len(names) > maxObserveSymbols ||
		*duration < time.Second || *duration > maxObserveDuration ||
		(*rotateAfter != 0 && (*rotateAfter < minRotateAfter || *rotateAfter > ws.DefaultRotateAfter)) {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, ok := loadConfig(*path, stderr)
	if !ok {
		return 1
	}

	w, syncOut, closeOut, err := openOutput(*out, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "observe:", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	code := observe(ctx, observeParams{cfg: cfg.MarketData, maxDataAge: cfg.Risk.MaxDataAge, names: names, duration: *duration,
		rotateAfter: *rotateAfter, baseURL: *baseURL, marketURL: *marketURL, wsURL: *wsURL, logger: logger, sync: syncOut}, w, stdout, stderr)
	if err := closeOut(); err != nil {
		fmt.Fprintln(stderr, "observe: close output:", err)
		return 1
	}
	return code
}

type observeParams struct {
	cfg                       config.MarketData
	maxDataAge                time.Duration
	names                     []string
	duration                  time.Duration
	rotateAfter               time.Duration
	baseURL, marketURL, wsURL string
	logger                    *slog.Logger
	sync                      func() error // flushes the output to stable storage; nil for none
}

func observe(ctx context.Context, p observeParams, w io.Writer, stdout, stderr io.Writer) int {
	rest, err := binance.New(binance.Options{BaseURL: p.baseURL, MarketDataURL: p.marketURL})
	if err != nil {
		fmt.Fprintln(stderr, "observe:", err)
		return 1
	}
	setupCtx, cancelSetup := context.WithTimeout(ctx, 30*time.Second)
	syms, err := rest.Symbols(setupCtx, p.names...)
	cancelSetup()
	if err != nil {
		fmt.Fprintln(stderr, "observe: exchangeInfo:", err)
		return 1
	}
	if missing := missingSymbols(p.names, syms); len(missing) > 0 {
		fmt.Fprintln(stderr, "observe: unknown symbols:", strings.Join(missing, ","))
		return 1
	}

	runCtx, cancel := context.WithTimeout(ctx, p.duration)
	defer cancel()
	md, err := marketdata.New(runCtx, marketdata.Config{Symbols: syms, Intervals: p.cfg.KlineIntervals,
		DepthLimit: p.cfg.DepthLimit, MaxDataAge: p.maxDataAge, Logger: p.logger}, rest, discardSink{})
	if err != nil {
		fmt.Fprintln(stderr, "observe:", err)
		return 1
	}
	mdCfg := marketdata.Config{Symbols: syms, Intervals: p.cfg.KlineIntervals}
	stream, err := ws.New(ws.Config{URL: p.wsURL, Streams: mdCfg.Streams(), MaxBackoff: p.cfg.ReconnectMaxBackoff,
		RotateAfter: p.rotateAfter, Logger: p.logger})
	if err != nil {
		fmt.Fprintln(stderr, "observe:", err)
		return 1
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = stream.Run(runCtx, md.Handle)
	}()

	enc := json.NewEncoder(w)
	start := time.Now()
	stale := map[string]time.Duration{} // measured by elapsed time, not by tick count
	var errs []string
	var errMu sync.Mutex
	lastTick := start
	second := time.NewTicker(time.Second)
	defer second.Stop()
	nextMinute := start.Add(time.Minute)
	writeErr := error(nil)
	var clk clockReport
	emit := func(r observeReport) {
		if writeErr != nil {
			return
		}
		if writeErr = enc.Encode(r); writeErr == nil && p.sync != nil {
			writeErr = p.sync()
		}
	}

	report := func(typ string, interrupted bool) observeReport {
		r := observeReport{Type: typ, Time: time.Now().UTC(), ElapsedSec: int64(time.Since(start).Seconds()),
			Interrupted: interrupted, WS: wsStats(stream.Stats()), Runtime: runtimeStats(), Clock: clk,
			Symbols: map[string]symbolReport{}, RateLimit: rest.Usage()}
		errMu.Lock()
		r.Errors = append([]string{}, errs...)
		errMu.Unlock()
		now := time.Now()
		for name, s := range md.Stats() {
			r.Symbols[name] = symbolReport{BookState: s.BookState, BookUpdateID: s.BookUpdateID, BookSnapshots: s.Book.Snapshots,
				BookGaps: s.Book.Gaps, BookCrossed: s.Book.Crossed, BookScaleUp: s.Book.ScaleRaises,
				BookBadValues: s.Book.BadValues, BookIgnored: s.Book.Ignored, SnapshotErrors: s.SnapshotErrs,
				Bars: s.Bars, BarsFromREST: s.BarsFromREST, BarGapsUnfill: s.BarGapsUnfill, Trades: s.Trades,
				TradeGaps: s.TradeGaps, Tickers: s.Tickers, StaleSeconds: int64(stale[name].Seconds()), LastEventAgeMs: ageMs(now, s.LastEvent)}
		}
		return r
	}

loop:
	for {
		select {
		case <-runCtx.Done():
			break loop
		case now := <-second.C:
			elapsed := now.Sub(lastTick)
			if j := clockJump(lastTick, now); j != 0 {
				clk.Jumps++
				clk.JumpSeconds += int64(j.Abs().Seconds())
				p.logger.Warn("wall clock jumped", "seconds", j.Seconds())
			}
			lastTick = now
			md.RetrySnapshots()
			for _, name := range md.Symbols() {
				if md.Stale(name) {
					stale[name] += elapsed
				}
			}
			if !now.Before(nextMinute) {
				nextMinute = nextMinute.Add(time.Minute)
				// One signed-host request per minute so both hosts report
				// X-MBX-USED-WEIGHT-1M (unknown K-9: shared or separate
				// counters). It runs in the background so a slow response
				// cannot delay the freshness sampling.
				wg.Add(1)
				go func() {
					defer wg.Done()
					tctx, tcancel := context.WithTimeout(runCtx, 10*time.Second)
					defer tcancel()
					if _, err := rest.ServerTime(tctx); err != nil && runCtx.Err() == nil {
						errMu.Lock()
						errs = appendBounded(errs, "server_time: "+err.Error())
						errMu.Unlock()
					}
				}()
				emit(report("minute", false))
			}
		}
	}
	interrupted := ctx.Err() != nil
	cancel()
	wg.Wait()
	md.Wait()
	final := report("final", interrupted)
	emit(final)
	if writeErr != nil {
		fmt.Fprintln(stderr, "observe: write report:", writeErr)
		return 1
	}
	fmt.Fprintln(stdout, summaryLine(final))
	return 0
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func splitSymbols(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		n = strings.ToUpper(strings.TrimSpace(n))
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func missingSymbols(want []string, got []exchange.Symbol) []string {
	var missing []string
	for _, n := range want {
		if !slices.ContainsFunc(got, func(s exchange.Symbol) bool { return s.Name == n }) {
			missing = append(missing, n)
		}
	}
	return missing
}

func wsStats(s *ws.Stats) wsReport {
	return wsReport{Dials: s.Dials.Load(), DialErrors: s.DialErrors.Load(), Disconnects: s.Disconnects.Load(),
		Rotations: s.Rotations.Load(), ShutdownEvents: s.ShutdownEvents.Load(), Messages: s.Messages.Load(), Malformed: s.Malformed.Load(),
		RotationFails: s.RotationFailures.Load(), Handovers: s.Handovers.Load(), RetiredDropped: s.RetiredDropped.Load(),
		QueueHighWater: s.QueueHighWater.Load(),
		MaxLagMs:       time.Duration(s.MaxDeliveryLagNs.Load()).Milliseconds()}
}

func runtimeStats() runtimeReport {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return runtimeReport{Goroutines: runtime.NumGoroutine(), HeapAllocBytes: ms.HeapAlloc, SysBytes: ms.Sys,
		NumGC: ms.NumGC, GCPauseTotalMs: time.Duration(ms.PauseTotalNs).Milliseconds()}
}

// clockJump returns how far the wall clock moved beyond the monotonic clock
// between prev and now, or 0 when the difference is within
// clockJumpThreshold. Both times must come from time.Now (with a monotonic
// reading).
func clockJump(prev, now time.Time) time.Duration {
	return jumpOf(now.Sub(prev), now.Round(0).Sub(prev.Round(0)))
}

func jumpOf(mono, wall time.Duration) time.Duration {
	if d := wall - mono; d.Abs() > clockJumpThreshold {
		return d
	}
	return 0
}

func ageMs(now, t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Milliseconds()
}

func appendBounded(errs []string, e string) []string {
	const maxErrors = 100
	if len(errs) >= maxErrors {
		return errs
	}
	if len(e) > 300 {
		e = e[:300] + "…"
	}
	return append(errs, e)
}

func summaryLine(r observeReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "observe %ds: ws dials=%d disconnects=%d rotations=%d shutdowns=%d malformed=%d",
		r.ElapsedSec, r.WS.Dials, r.WS.Disconnects, r.WS.Rotations, r.WS.ShutdownEvents, r.WS.Malformed)
	names := make([]string, 0, len(r.Symbols))
	for n := range r.Symbols {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		s := r.Symbols[n]
		fmt.Fprintf(&b, "\n  %s book=%s gaps=%d crossed=%d bars=%d rest_filled=%d unfilled=%d trades=%d stale_s=%d",
			n, s.BookState, s.BookGaps, s.BookCrossed, s.Bars, s.BarsFromREST, s.BarGapsUnfill, s.Trades, s.StaleSeconds)
	}
	return b.String()
}

// openOutput returns stdout or a newly created file, a sync function (nil
// for stdout) and a close function that reports the file's close error.
// Each report line is synced so that a crash or power loss keeps every line
// written before it.
func openOutput(path string, stdout io.Writer) (io.Writer, func() error, func() error, error) {
	if path == "" {
		return stdout, nil, func() error { return nil }, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, nil, err
	}
	// Sync the directory once so the new file's entry survives a power loss.
	// Windows cannot sync a directory handle; there the file sync suffices.
	if runtime.GOOS != "windows" {
		if err := syncDir(filepath.Dir(path)); err != nil {
			f.Close()
			return nil, nil, nil, fmt.Errorf("sync directory of %s: %w", path, err)
		}
	}
	return f, f.Sync, f.Close, nil
}
