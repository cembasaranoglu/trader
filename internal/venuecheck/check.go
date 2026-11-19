// Package venuecheck measures the live Binance TR facts that the design
// documents could not verify (wave W1): clock skew, symbol rules for TRY
// pairs, market-data response shapes, rate-limit headers and account flags.
//
// The report contains no secrets and no balance amounts, so it can be shared.
package venuecheck

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binancetr"
)

// Venue is the subset of the Binance TR client that the check needs.
type Venue interface {
	ServerTime(ctx context.Context) (time.Time, error)
	Symbols(ctx context.Context) ([]exchange.Symbol, error)
	Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, binancetr.Shape, error)
	Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, binancetr.Shape, error)
	Account(ctx context.Context) (exchange.AccountInfo, error)
	Usage() map[string]string
}

// Options configures a check run.
type Options struct {
	Symbols      []string         // venue names to inspect, e.g. "BTC_TRY"
	ClockSamples int              // server time samples; >= 1
	CheckAccount bool             // call the signed account endpoint
	Now          func() time.Time // nil: time.Now
	// Pause between requests; keeps the check far below any rate limit.
	Pause time.Duration
}

// Report is the shareable result of a check.
type Report struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	Clock            *ClockReport      `json:"clock,omitempty"`
	Symbols          *SymbolsSummary   `json:"symbols,omitempty"`
	Watched          []SymbolReport    `json:"watched"`
	RateLimitHeaders map[string]string `json:"rate_limit_headers"`
	Account          *AccountReport    `json:"account,omitempty"`
	AccountSkipped   string            `json:"account_skipped,omitempty"`
	Errors           []string          `json:"errors"`
	ManualChecks     []string          `json:"manual_checks"`
}

// ClockReport summarizes local vs. exchange clock offset.
type ClockReport struct {
	Samples        int   `json:"samples"`
	MedianOffsetMs int64 `json:"median_offset_ms"` // server minus local; positive means local is behind
	MaxRTTMs       int64 `json:"max_rtt_ms"`
}

// SymbolsSummary counts symbols by quote and routing type.
type SymbolsSummary struct {
	Total          int         `json:"total"`
	ByType         map[int]int `json:"by_type"`
	TRYQuoted      int         `json:"try_quoted"`
	TRYSpotEnabled int         `json:"try_spot_enabled"`
	TRYByType      map[int]int `json:"try_by_type"`
	NonTRYQuoted   int         `json:"non_try_quoted"`
}

// SymbolReport holds the measured rules and market data for one symbol.
type SymbolReport struct {
	Name                    string   `json:"name"`
	Found                   bool     `json:"found"`
	Type                    int      `json:"type,omitempty"`
	SpotEnabled             bool     `json:"spot_enabled,omitempty"`
	OCOEnabled              bool     `json:"oco_enabled,omitempty"`
	OrderTypes              []string `json:"order_types,omitempty"`
	STPDefault              string   `json:"stp_default,omitempty"`
	STPAllowed              []string `json:"stp_allowed,omitempty"`
	TickSize                string   `json:"tick_size,omitempty"`
	StepSize                string   `json:"step_size,omitempty"`
	MinQty                  string   `json:"min_qty,omitempty"`
	MinNotional             string   `json:"min_notional,omitempty"`
	NotionalAppliesToMarket bool     `json:"notional_applies_to_market,omitempty"`
	DepthShape              string   `json:"depth_shape,omitempty"`
	BestBid                 string   `json:"best_bid,omitempty"`
	BestAsk                 string   `json:"best_ask,omitempty"`
	SpreadBps               *float64 `json:"spread_bps,omitempty"`
	KlinesShape             string   `json:"klines_shape,omitempty"`
	LastKlineCloseAgeSec    *float64 `json:"last_kline_close_age_sec,omitempty"`
}

// AccountReport holds account flags and fee rates. Balance amounts are omitted.
type AccountReport struct {
	CanTrade           bool   `json:"can_trade"`
	CanWithdraw        bool   `json:"can_withdraw_account_level"`
	CanDeposit         bool   `json:"can_deposit"`
	FiatMakerRate      string `json:"fiat_maker_rate,omitempty"`
	FiatTakerRate      string `json:"fiat_taker_rate,omitempty"`
	RawMakerCommission string `json:"raw_maker_commission,omitempty"`
	RawTakerCommission string `json:"raw_taker_commission,omitempty"`
	AssetCount         int    `json:"asset_count"`
}

// manualChecks lists facts the API cannot reveal.
var manualChecks = []string{
	"API key permissions (read / trade / withdraw) are not exposed by the API: confirm in the Binance TR web UI that withdraw is disabled.",
	"IP whitelist support is not exposed by the API: check whether the API management page offers an IP restriction option.",
	"Delisting and maintenance announcements: note where Binance TR publishes them (web page, e-mail, API).",
}

// Run performs the check. Individual failures are recorded in Report.Errors;
// Run returns an error only when ctx is done.
func Run(ctx context.Context, v Venue, opts Options) (*Report, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.ClockSamples < 1 {
		opts.ClockSamples = 1
	}
	rep := &Report{GeneratedAt: now().UTC(), Watched: []SymbolReport{}, Errors: []string{}, ManualChecks: manualChecks}
	fail := func(step string, err error) { rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", step, err)) }
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
			mid := t0.Add(rtt / 2)
			offsets = append(offsets, st.Sub(mid).Milliseconds())
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
	syms, err := v.Symbols(ctx)
	if err != nil {
		fail("symbols", err)
	}
	byName := make(map[string]exchange.Symbol, len(syms))
	if err == nil {
		sum := &SymbolsSummary{Total: len(syms), ByType: map[int]int{}, TRYByType: map[int]int{}}
		for _, s := range syms {
			byName[s.Name] = s
			sum.ByType[s.Type]++
			if s.Quote == "TRY" {
				sum.TRYQuoted++
				sum.TRYByType[s.Type]++
				if s.SpotEnabled {
					sum.TRYSpotEnabled++
				}
			} else {
				sum.NonTRYQuoted++
			}
		}
		rep.Symbols = sum
	}
	if err := pause(); err != nil {
		return rep, err
	}

	// Watched symbols: rules, depth, klines.
	for _, name := range opts.Symbols {
		sr := SymbolReport{Name: name}
		s, ok := byName[name]
		if !ok {
			rep.Watched = append(rep.Watched, sr)
			continue
		}
		sr.Found, sr.Type, sr.SpotEnabled, sr.OCOEnabled = true, s.Type, s.SpotEnabled, s.OCOEnabled
		sr.OrderTypes, sr.STPDefault, sr.STPAllowed = s.OrderTypes, s.STPDefault, s.STPAllowed
		sr.TickSize, sr.StepSize = dec(s.Filters.TickSize), dec(s.Filters.StepSize)
		sr.MinQty, sr.MinNotional = dec(s.Filters.MinQty), dec(s.Filters.MinNotional)
		sr.NotionalAppliesToMarket = s.Filters.NotionalAppliesToMarket

		ob, shape, err := v.Depth(ctx, s, 5)
		if err != nil {
			fail("depth "+name, err)
		} else {
			sr.DepthShape = string(shape)
			if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
				sr.BestBid, sr.BestAsk = dec(ob.Bids[0].Price), dec(ob.Asks[0].Price)
				sr.SpreadBps = spreadBps(ob.Bids[0].Price, ob.Asks[0].Price)
			}
		}
		if err := pause(); err != nil {
			return rep, err
		}
		ks, shape, err := v.Klines(ctx, s, "1m", 2)
		if err != nil {
			fail("klines "+name, err)
		} else {
			sr.KlinesShape = string(shape)
			if n := len(ks); n > 0 {
				age := now().Sub(ks[n-1].CloseTime).Seconds()
				sr.LastKlineCloseAgeSec = &age
			}
		}
		rep.Watched = append(rep.Watched, sr)
		if err := pause(); err != nil {
			return rep, err
		}
	}

	// Account (signed).
	if !opts.CheckAccount {
		rep.AccountSkipped = "no credentials file given"
	} else if info, err := v.Account(ctx); err != nil {
		fail("account", err)
	} else {
		rep.Account = &AccountReport{
			CanTrade: info.CanTrade, CanWithdraw: info.CanWithdraw, CanDeposit: info.CanDeposit,
			FiatMakerRate: dec(info.FiatMakerRate), FiatTakerRate: dec(info.FiatTakerRate),
			RawMakerCommission: info.RawMakerCommission, RawTakerCommission: info.RawTakerCommission,
			AssetCount: len(info.Balances),
		}
	}
	rep.RateLimitHeaders = v.Usage()
	return rep, nil
}

func median(xs []int64) int64 {
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
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
	diff := new(big.Rat).Sub(ask, bid)
	bps := new(big.Rat).Quo(diff, mid)
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
