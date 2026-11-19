// Package venuecheck measures live exchange facts that documentation alone
// cannot settle: clock skew, symbol rules, liquidity ranking, rate-limit
// headers, account flags and API key permissions.
//
// The report contains no secrets and no balance amounts, so it can be shared.
// Every string that originates from the exchange is sanitized (control
// characters removed, invalid UTF-8 replaced, length bounded) before it is
// written to the report.
package venuecheck

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

// Venue is the read-side surface the check needs from an exchange adapter.
type Venue interface {
	ServerTime(ctx context.Context) (time.Time, error)
	Symbols(ctx context.Context) ([]exchange.Symbol, error)
	Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, error)
	Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, error)
	Account(ctx context.Context) (exchange.AccountInfo, error)
	Usage() map[string]string
}

// TickerSource is implemented by venues that publish 24h tickers.
type TickerSource interface {
	Tickers24h(ctx context.Context) ([]exchange.Ticker, error)
}

// PermissionSource is implemented by venues that expose API key permissions.
type PermissionSource interface {
	KeyPermissions(ctx context.Context) (exchange.KeyPermissions, error)
}

// ClockSyncer is implemented by venues that can align signed request
// timestamps with the exchange clock before signed calls.
type ClockSyncer interface {
	SyncTime(ctx context.Context) (time.Duration, error)
}

// Options configures a check run.
type Options struct {
	Venue        string   // label for the report, e.g. "binance"
	Quote        string   // quote asset to summarize, e.g. "USDT"
	Symbols      []string // venue names to inspect in detail
	TopN         int      // size of the liquidity ranking; 0 disables it
	ClockSamples int
	CheckAccount bool // call signed endpoints
	Now          func() time.Time
	Pause        time.Duration // between requests
}

// Report is the shareable result of a check.
type Report struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	Venue            string            `json:"venue"`
	Clock            *ClockReport      `json:"clock,omitempty"`
	Symbols          *SymbolsSummary   `json:"symbols,omitempty"`
	TopByQuoteVolume []TopSymbol       `json:"top_by_quote_volume_24h,omitempty"`
	Watched          []SymbolReport    `json:"watched"`
	RateLimitHeaders map[string]string `json:"rate_limit_headers"`
	Account          *AccountReport    `json:"account,omitempty"`
	KeyPermissions   *PermissionReport `json:"key_permissions,omitempty"`
	AccountSkipped   string            `json:"account_skipped,omitempty"`
	Warnings         []string          `json:"warnings"`
	Errors           []string          `json:"errors"`
	ManualChecks     []string          `json:"manual_checks"`
}

// ClockReport summarizes local vs. exchange clock offset.
type ClockReport struct {
	Samples        int   `json:"samples"`
	MedianOffsetMs int64 `json:"median_offset_ms"` // server minus local; positive means local is behind
	MaxRTTMs       int64 `json:"max_rtt_ms"`
}

// SymbolsSummary counts symbols for the configured quote asset.
type SymbolsSummary struct {
	Total            int         `json:"total"`
	Quote            string      `json:"quote"`
	QuoteCount       int         `json:"quote_count"`
	QuoteSpotEnabled int         `json:"quote_spot_enabled"`
	ByType           map[int]int `json:"by_type,omitempty"` // only for venues with routing types
}

// TopSymbol is one entry of the 24h liquidity ranking.
type TopSymbol struct {
	Symbol         string `json:"symbol"`
	QuoteVolume24h string `json:"quote_volume_24h"`
	LastPrice      string `json:"last_price"`
}

// SymbolReport holds the measured rules and market data for one symbol.
type SymbolReport struct {
	Name                    string   `json:"name"`
	Found                   bool     `json:"found"`
	Status                  string   `json:"status,omitempty"`
	SpotEnabled             bool     `json:"spot_enabled,omitempty"`
	OCOEnabled              bool     `json:"oco_enabled,omitempty"`
	OrderTypes              []string `json:"order_types,omitempty"`
	STPDefault              string   `json:"stp_default,omitempty"`
	STPAllowed              []string `json:"stp_allowed,omitempty"`
	TickSize                string   `json:"tick_size,omitempty"`
	StepSize                string   `json:"step_size,omitempty"`
	MinQty                  string   `json:"min_qty,omitempty"`
	MinNotional             string   `json:"min_notional,omitempty"`
	MaxNotional             string   `json:"max_notional,omitempty"`
	NotionalAppliesToMarket bool     `json:"notional_applies_to_market"`
	BestBid                 string   `json:"best_bid,omitempty"`
	BestAsk                 string   `json:"best_ask,omitempty"`
	SpreadBps               *float64 `json:"spread_bps,omitempty"`
	LastClosedKlineAgeSec   *float64 `json:"last_closed_kline_age_sec,omitempty"`
}

// AccountReport holds account flags and fee rates. Balance amounts are omitted.
type AccountReport struct {
	CanTrade           bool   `json:"can_trade"`
	CanWithdraw        bool   `json:"can_withdraw_account_level"`
	CanDeposit         bool   `json:"can_deposit"`
	MakerRate          string `json:"maker_rate,omitempty"`
	TakerRate          string `json:"taker_rate,omitempty"`
	RawMakerCommission string `json:"raw_maker_commission,omitempty"`
	RawTakerCommission string `json:"raw_taker_commission,omitempty"`
	AssetCount         int    `json:"asset_count"`
}

// PermissionReport mirrors exchange.KeyPermissions.
type PermissionReport struct {
	IPRestricted bool     `json:"ip_restricted"`
	Reading      bool     `json:"reading"`
	SpotTrading  bool     `json:"spot_trading"`
	Withdrawals  bool     `json:"withdrawals"`
	OtherEnabled []string `json:"other_enabled"`
}

// Run performs the check. Individual failures go to Report.Errors; Run
// returns an error only when ctx is done.
func Run(ctx context.Context, v Venue, opts Options) (*Report, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.ClockSamples < 1 {
		opts.ClockSamples = 1
	}
	_, hasPerms := v.(PermissionSource)
	rep := &Report{GeneratedAt: now().UTC(), Venue: opts.Venue, Watched: []SymbolReport{},
		Warnings: []string{}, Errors: []string{}, ManualChecks: manualChecks(hasPerms)}
	fail := func(step string, err error) {
		rep.Errors = append(rep.Errors, clean(fmt.Sprintf("%s: %v", step, err), maxErrorLen))
	}
	pause := func() error {
		if opts.Pause <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(opts.Pause)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}

	// Clock skew.
	var offsets []int64
	var maxRTT time.Duration
	for i := 0; i < opts.ClockSamples; i++ {
		t0 := now()
		st, err := v.ServerTime(ctx)
		t1 := now()
		if err != nil {
			fail("server_time", err)
		} else {
			rtt := t1.Sub(t0)
			offsets = append(offsets, st.Sub(t0.Add(rtt/2)).Milliseconds())
			if rtt > maxRTT {
				maxRTT = rtt
			}
		}
		if err := pause(); err != nil {
			return rep, err
		}
	}
	if len(offsets) > 0 {
		rep.Clock = &ClockReport{Samples: len(offsets), MedianOffsetMs: median(offsets), MaxRTTMs: maxRTT.Milliseconds()}
	}

	// Symbol rules.
	byName := map[string]exchange.Symbol{}
	if syms, err := v.Symbols(ctx); err != nil {
		fail("symbols", err)
	} else {
		sum := &SymbolsSummary{Total: len(syms), Quote: opts.Quote, ByType: map[int]int{}}
		for _, s := range syms {
			byName[s.Name] = s
			if s.Type != 0 {
				sum.ByType[s.Type]++
			}
			if s.Quote == opts.Quote {
				sum.QuoteCount++
				if s.SpotEnabled {
					sum.QuoteSpotEnabled++
				}
			}
		}
		if len(sum.ByType) == 0 {
			sum.ByType = nil
		}
		rep.Symbols = sum
	}
	if err := pause(); err != nil {
		return rep, err
	}

	// Liquidity ranking for the quote asset.
	if ts, ok := v.(TickerSource); ok && opts.TopN > 0 {
		if tickers, err := ts.Tickers24h(ctx); err != nil {
			fail("tickers_24h", err)
		} else {
			rep.TopByQuoteVolume = topByVolume(tickers, byName, opts.Quote, opts.TopN)
		}
		if err := pause(); err != nil {
			return rep, err
		}
	}

	// Watched symbols.
	for _, name := range opts.Symbols {
		sr := SymbolReport{Name: clean(name, maxFieldLen)}
		s, ok := byName[name]
		if !ok {
			rep.Watched = append(rep.Watched, sr)
			continue
		}
		sr.Found, sr.Status, sr.SpotEnabled, sr.OCOEnabled = true, clean(s.Status, maxFieldLen), s.SpotEnabled, s.OCOEnabled
		sr.OrderTypes, sr.STPDefault, sr.STPAllowed = cleanAll(s.OrderTypes), clean(s.STPDefault, maxFieldLen), cleanAll(s.STPAllowed)
		sr.TickSize, sr.StepSize = dec(s.Filters.TickSize), dec(s.Filters.StepSize)
		sr.MinQty, sr.MinNotional, sr.MaxNotional = dec(s.Filters.MinQty), dec(s.Filters.MinNotional), dec(s.Filters.MaxNotional)
		sr.NotionalAppliesToMarket = s.Filters.NotionalAppliesToMarket

		if ob, err := v.Depth(ctx, s, 5); err != nil {
			fail("depth "+name, err)
		} else if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
			sr.BestBid, sr.BestAsk = dec(ob.Bids[0].Price), dec(ob.Asks[0].Price)
			sr.SpreadBps = spreadBps(ob.Bids[0].Price, ob.Asks[0].Price)
		}
		if err := pause(); err != nil {
			return rep, err
		}
		if ks, err := v.Klines(ctx, s, "1m", 2); err != nil {
			fail("klines "+name, err)
		} else {
			// The newest kline is usually still open; use the newest closed one.
			t := now()
			for i := len(ks) - 1; i >= 0; i-- {
				if !ks[i].CloseTime.After(t) {
					age := t.Sub(ks[i].CloseTime).Seconds()
					sr.LastClosedKlineAgeSec = &age
					break
				}
			}
		}
		rep.Watched = append(rep.Watched, sr)
		if err := pause(); err != nil {
			return rep, err
		}
	}

	// Signed endpoints.
	if !opts.CheckAccount {
		rep.AccountSkipped = "no credentials file given"
	} else {
		if cs, ok := v.(ClockSyncer); ok {
			if _, err := cs.SyncTime(ctx); err != nil {
				fail("clock_sync", err)
			}
			if err := pause(); err != nil {
				return rep, err
			}
		}
		if info, err := v.Account(ctx); err != nil {
			fail("account", err)
		} else {
			rep.Account = &AccountReport{
				CanTrade: info.CanTrade, CanWithdraw: info.CanWithdraw, CanDeposit: info.CanDeposit,
				MakerRate: dec(info.MakerRate), TakerRate: dec(info.TakerRate),
				RawMakerCommission: clean(info.RawMakerCommission, maxFieldLen), RawTakerCommission: clean(info.RawTakerCommission, maxFieldLen),
				AssetCount: len(info.Balances),
			}
		}
		if ps, ok := v.(PermissionSource); ok {
			if err := pause(); err != nil {
				return rep, err
			}
			if p, err := ps.KeyPermissions(ctx); err != nil {
				fail("key_permissions", err)
			} else {
				rep.KeyPermissions = &PermissionReport{IPRestricted: p.IPRestricted, Reading: p.Reading,
					SpotTrading: p.SpotTrading, Withdrawals: p.Withdrawals, OtherEnabled: cleanAll(p.OtherEnabled)}
				rep.Warnings = append(rep.Warnings, permissionWarnings(p)...)
			}
		}
	}
	rep.RateLimitHeaders = map[string]string{}
	for k, val := range v.Usage() {
		rep.RateLimitHeaders[clean(k, maxFieldLen)] = clean(val, maxFieldLen)
	}
	return rep, nil
}

// Report string bounds.
const (
	maxFieldLen = 128
	maxErrorLen = 512
)

// clean makes an untrusted string safe for the report: invalid UTF-8 becomes
// "?", control characters are dropped and the result is cut to limit bytes on
// a rune boundary.
func clean(s string, limit int) string {
	s = strings.ToValidUTF8(s, "?")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+len(string(r)) > limit {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cleanAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, clean(s, maxFieldLen))
	}
	return out
}

// permissionWarnings flags permissions the design forbids (I-7) or limits.
func permissionWarnings(p exchange.KeyPermissions) []string {
	var w []string
	if p.Withdrawals {
		w = append(w, "API key has WITHDRAWALS enabled: disable it now (invariant I-7)")
	}
	if p.SpotTrading && !p.IPRestricted {
		w = append(w, "API key can trade without an IP restriction: restrict it to the server IP")
	}
	if len(p.OtherEnabled) > 0 {
		w = append(w, fmt.Sprintf("API key has extra permissions enabled: %v; only reading (and later spot trading) is needed", cleanAll(p.OtherEnabled)))
	}
	return w
}

func manualChecks(hasPerms bool) []string {
	out := []string{"Delisting and maintenance announcements: note where the venue publishes them (web page, e-mail, API)."}
	if !hasPerms {
		out = append([]string{
			"API key permissions (read / trade / withdraw) are not exposed by this venue's API: confirm in the web UI that withdraw is disabled.",
			"IP whitelist support is not exposed by this venue's API: check the API management page.",
		}, out...)
	}
	return out
}

// topByVolume ranks spot-enabled symbols of the quote asset by 24h quote volume.
func topByVolume(ts []exchange.Ticker, byName map[string]exchange.Symbol, quote string, n int) []TopSymbol {
	var cand []exchange.Ticker
	for _, t := range ts {
		s, ok := byName[t.Symbol]
		if ok && s.Quote == quote && s.SpotEnabled && t.QuoteVolume != nil {
			cand = append(cand, t)
		}
	}
	// Highest volume first; equal volumes are ordered by symbol name so the
	// ranking is deterministic.
	slices.SortStableFunc(cand, func(a, b exchange.Ticker) int {
		if c := b.QuoteVolume.Cmp(a.QuoteVolume); c != 0 {
			return c
		}
		return strings.Compare(a.Symbol, b.Symbol)
	})
	if len(cand) > n {
		cand = cand[:n]
	}
	out := make([]TopSymbol, 0, len(cand))
	for _, t := range cand {
		out = append(out, TopSymbol{Symbol: clean(t.Symbol, maxFieldLen), QuoteVolume24h: t.QuoteVolume.FloatString(0), LastPrice: dec(t.LastPrice)})
	}
	return out
}

func median(xs []int64) int64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// spreadBps returns (ask - bid) / mid * 10000, or nil for a crossed or empty book.
func spreadBps(bid, ask *big.Rat) *float64 {
	if bid == nil || ask == nil || bid.Sign() <= 0 || ask.Cmp(bid) < 0 {
		return nil
	}
	mid := new(big.Rat).Add(bid, ask)
	mid.Quo(mid, big.NewRat(2, 1))
	bps := new(big.Rat).Sub(ask, bid)
	bps.Quo(bps, mid)
	bps.Mul(bps, big.NewRat(10000, 1))
	f, _ := bps.Float64() // reporting only
	return &f
}

// dec formats a decimal without trailing zeros; empty for nil.
func dec(r *big.Rat) string {
	if r == nil {
		return ""
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(18)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	return s
}
