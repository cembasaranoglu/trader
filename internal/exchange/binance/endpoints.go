package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

// ServerTime returns the clock of the signing host (weight 1). Request
// timestamps are validated against this host, so it is the clock to sync to.
func (c *Client) ServerTime(ctx context.Context) (time.Time, error) {
	var v struct {
		ServerTime *int64 `json:"serverTime"`
	}
	if err := c.get(ctx, request{host: c.base, path: "/api/v3/time", weight: 1}, &v); err != nil {
		return time.Time{}, err
	}
	if v.ServerTime == nil || *v.ServerTime <= 0 {
		return time.Time{}, fmt.Errorf("%w: serverTime missing", ErrBadResponse)
	}
	return time.UnixMilli(*v.ServerTime), nil
}

type wireFilter struct {
	FilterType       string `json:"filterType"`
	TickSize         string `json:"tickSize"`
	MinPrice         string `json:"minPrice"`
	MaxPrice         string `json:"maxPrice"`
	StepSize         string `json:"stepSize"`
	MinQty           string `json:"minQty"`
	MaxQty           string `json:"maxQty"`
	MinNotional      string `json:"minNotional"`
	MaxNotional      string `json:"maxNotional"`
	ApplyToMarket    *bool  `json:"applyToMarket"`    // MIN_NOTIONAL
	ApplyMinToMarket *bool  `json:"applyMinToMarket"` // NOTIONAL
	ApplyMaxToMarket *bool  `json:"applyMaxToMarket"` // NOTIONAL
}

type wireSymbol struct {
	Symbol      *string      `json:"symbol"`
	Status      *string      `json:"status"`
	BaseAsset   *string      `json:"baseAsset"`
	QuoteAsset  *string      `json:"quoteAsset"`
	OrderTypes  []string     `json:"orderTypes"`
	OCOAllowed  *bool        `json:"ocoAllowed"`
	SpotAllowed *bool        `json:"isSpotTradingAllowed"`
	DefaultSTP  string       `json:"defaultSelfTradePreventionMode"`
	AllowedSTP  []string     `json:"allowedSelfTradePreventionModes"`
	Filters     []wireFilter `json:"filters"`
}

// Symbols returns spot symbols with their filters (weight 20). With names
// empty it returns every symbol. A symbol with a missing required field fails
// the whole call: partial symbol rules are never returned.
func (c *Client) Symbols(ctx context.Context, names ...string) ([]exchange.Symbol, error) {
	q := url.Values{"showPermissionSets": {"false"}}
	if len(names) > 0 {
		b, err := json.Marshal(names)
		if err != nil {
			return nil, err
		}
		q.Set("symbols", string(b))
	}
	var v struct {
		RateLimits []struct {
			Type        string `json:"rateLimitType"`
			Interval    string `json:"interval"`
			IntervalNum int    `json:"intervalNum"`
			Limit       int    `json:"limit"`
		} `json:"rateLimits"`
		Symbols []wireSymbol `json:"symbols"`
	}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/exchangeInfo", query: q, weight: 20}, &v); err != nil {
		return nil, err
	}
	for _, rl := range v.RateLimits {
		if rl.Type == "REQUEST_WEIGHT" && rl.Interval == "MINUTE" && rl.IntervalNum == 1 {
			c.SetRequestWeightLimit(rl.Limit)
		}
	}
	if len(v.Symbols) == 0 {
		return nil, fmt.Errorf("%w: exchangeInfo has no symbols", ErrBadResponse)
	}
	out := make([]exchange.Symbol, 0, len(v.Symbols))
	for i, w := range v.Symbols {
		if w.Symbol == nil || *w.Symbol == "" || w.Status == nil || w.BaseAsset == nil || w.QuoteAsset == nil ||
			w.OCOAllowed == nil || w.SpotAllowed == nil || w.OrderTypes == nil || w.Filters == nil {
			return nil, fmt.Errorf("%w: exchangeInfo symbol %d lacks required fields", ErrBadResponse, i)
		}
		f, err := parseFilters(w.Filters)
		if err != nil {
			return nil, fmt.Errorf("%w: symbol %s: %v", ErrBadResponse, *w.Symbol, err)
		}
		out = append(out, exchange.Symbol{
			Name: *w.Symbol, Base: *w.BaseAsset, Quote: *w.QuoteAsset, Status: *w.Status,
			SpotEnabled: *w.Status == "TRADING" && *w.SpotAllowed, OCOEnabled: *w.OCOAllowed,
			OrderTypes: w.OrderTypes, STPDefault: w.DefaultSTP, STPAllowed: w.AllowedSTP, Filters: f,
		})
	}
	return out, nil
}

// parseFilters maps Binance symbol filters.
//
// PRICE_FILTER: Binance documents that minPrice, maxPrice and tickSize may be
// 0, which disables that rule (binance-spot-api-docs filters.md); 0 is
// therefore stored as nil. Other filters keep 0 as published.
// NOTIONAL takes precedence over MIN_NOTIONAL regardless of order.
func parseFilters(ws []wireFilter) (exchange.Filters, error) {
	var f exchange.Filters
	dec := func(field, s string) (*big.Rat, error) {
		if s == "" {
			return nil, fmt.Errorf("%s missing", field)
		}
		r, err := exchange.ParseDecimal(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return r, nil
	}
	nonZero := func(r *big.Rat) *big.Rat {
		if r == nil || r.Sign() == 0 {
			return nil
		}
		return r
	}
	var sawNotional bool
	for _, w := range ws {
		var err error
		switch w.FilterType {
		case "PRICE_FILTER":
			var tick, lo, hi *big.Rat
			if tick, err = dec("tickSize", w.TickSize); err == nil {
				if lo, err = dec("minPrice", w.MinPrice); err == nil {
					hi, err = dec("maxPrice", w.MaxPrice)
				}
			}
			f.TickSize, f.MinPrice, f.MaxPrice = nonZero(tick), nonZero(lo), nonZero(hi)
		case "LOT_SIZE":
			if f.StepSize, err = dec("stepSize", w.StepSize); err == nil {
				if f.MinQty, err = dec("minQty", w.MinQty); err == nil {
					f.MaxQty, err = dec("maxQty", w.MaxQty)
				}
			}
			f.StepSize = nonZero(f.StepSize) // a zero step cannot define a grid
		case "NOTIONAL":
			sawNotional = true
			if w.ApplyMinToMarket == nil || w.ApplyMaxToMarket == nil {
				err = fmt.Errorf("applyMinToMarket/applyMaxToMarket missing")
				break
			}
			if f.MinNotional, err = dec("minNotional", w.MinNotional); err == nil {
				f.MaxNotional, err = dec("maxNotional", w.MaxNotional)
			}
			f.NotionalAppliesToMarket = *w.ApplyMinToMarket
			f.MaxNotionalAppliesToMarket = *w.ApplyMaxToMarket
		case "MIN_NOTIONAL":
			if sawNotional {
				continue
			}
			if w.ApplyToMarket == nil {
				err = fmt.Errorf("applyToMarket missing")
				break
			}
			f.MinNotional, err = dec("minNotional", w.MinNotional)
			f.NotionalAppliesToMarket = *w.ApplyToMarket
		}
		if err != nil {
			return f, fmt.Errorf("filter %s: %w", w.FilterType, err)
		}
	}
	return f, nil
}

// Tickers24h returns the rolling 24h summary of every symbol (weight 80).
func (c *Client) Tickers24h(ctx context.Context) ([]exchange.Ticker, error) {
	var v []struct {
		Symbol      string `json:"symbol"`
		LastPrice   string `json:"lastPrice"`
		QuoteVolume string `json:"quoteVolume"`
	}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/ticker/24hr", query: url.Values{"type": {"MINI"}}, weight: 80}, &v); err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("%w: no tickers", ErrBadResponse)
	}
	out := make([]exchange.Ticker, 0, len(v))
	for i, w := range v {
		if w.Symbol == "" {
			return nil, fmt.Errorf("%w: ticker %d has no symbol", ErrBadResponse, i)
		}
		lp, err := exchange.ParseDecimal(w.LastPrice)
		if err != nil {
			return nil, fmt.Errorf("%w: ticker %s: lastPrice: %v", ErrBadResponse, w.Symbol, err)
		}
		qv, err := exchange.ParseDecimal(w.QuoteVolume)
		if err != nil {
			return nil, fmt.Errorf("%w: ticker %s: quoteVolume: %v", ErrBadResponse, w.Symbol, err)
		}
		out = append(out, exchange.Ticker{Symbol: w.Symbol, LastPrice: lp, QuoteVolume: qv})
	}
	return out, nil
}

// Depth returns an order book snapshot (limit 1..5000).
func (c *Client) Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, error) {
	if limit < 1 || limit > 5000 {
		return exchange.OrderBook{}, fmt.Errorf("binance: depth limit %d not in [1, 5000]", limit)
	}
	var w struct {
		LastUpdateID *int64      `json:"lastUpdateId"`
		Bids         [][2]string `json:"bids"`
		Asks         [][2]string `json:"asks"`
	}
	q := url.Values{"symbol": {s.Name}, "limit": {strconv.Itoa(limit)}}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/depth", query: q, weight: DepthWeight(limit)}, &w); err != nil {
		return exchange.OrderBook{}, err
	}
	if w.LastUpdateID == nil || w.Bids == nil || w.Asks == nil {
		return exchange.OrderBook{}, fmt.Errorf("%w: depth lacks lastUpdateId, bids or asks", ErrBadResponse)
	}
	ob := exchange.OrderBook{LastUpdateID: *w.LastUpdateID}
	var err error
	if ob.Bids, err = levels(w.Bids); err != nil {
		return exchange.OrderBook{}, fmt.Errorf("%w: bids: %v", ErrBadResponse, err)
	}
	if ob.Asks, err = levels(w.Asks); err != nil {
		return exchange.OrderBook{}, fmt.Errorf("%w: asks: %v", ErrBadResponse, err)
	}
	return ob, nil
}

// DepthWeight returns the documented request weight of GET /api/v3/depth.
func DepthWeight(limit int) int {
	switch {
	case limit <= 100:
		return 5
	case limit <= 500:
		return 25
	case limit <= 1000:
		return 50
	default:
		return 250
	}
}

func levels(raw [][2]string) ([]exchange.Level, error) {
	out := make([]exchange.Level, 0, len(raw))
	for _, l := range raw {
		p, err := exchange.ParseDecimal(l[0])
		if err != nil {
			return nil, err
		}
		q, err := exchange.ParseDecimal(l[1])
		if err != nil {
			return nil, err
		}
		out = append(out, exchange.Level{Price: p, Qty: q})
	}
	return out, nil
}

// klineIntervals are the intervals Binance spot accepts (enums.md).
var klineIntervals = []string{"1s", "1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d", "3d", "1w", "1M"}

// Klines returns up to limit candles (1..1000), oldest first. The newest one
// is usually still open.
func (c *Client) Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("binance: klines limit %d not in [1, 1000]", limit)
	}
	if !slices.Contains(klineIntervals, interval) {
		return nil, fmt.Errorf("binance: unsupported klines interval %q", interval)
	}
	q := url.Values{"symbol": {s.Name}, "interval": {interval}, "limit": {strconv.Itoa(limit)}}
	return c.klines(ctx, q)
}

// KlinesRange returns candles whose open time is in [start, end], oldest
// first, at most limit (1..1000). Klines are identified by their open time.
func (c *Client) KlinesRange(ctx context.Context, s exchange.Symbol, interval string, start, end time.Time, limit int) ([]exchange.Kline, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("binance: klines limit %d not in [1, 1000]", limit)
	}
	if !slices.Contains(klineIntervals, interval) {
		return nil, fmt.Errorf("binance: unsupported klines interval %q", interval)
	}
	if start.IsZero() || end.Before(start) {
		return nil, fmt.Errorf("binance: invalid klines range %s..%s", start, end)
	}
	q := url.Values{"symbol": {s.Name}, "interval": {interval}, "limit": {strconv.Itoa(limit)},
		"startTime": {strconv.FormatInt(start.UnixMilli(), 10)}, "endTime": {strconv.FormatInt(end.UnixMilli(), 10)}}
	return c.klines(ctx, q)
}

func (c *Client) klines(ctx context.Context, q url.Values) ([]exchange.Kline, error) {
	var rows [][]json.RawMessage
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/klines", query: q, weight: 2}, &rows); err != nil {
		return nil, err
	}
	out := make([]exchange.Kline, 0, len(rows))
	for i, row := range rows {
		k, err := parseKline(row)
		if err != nil {
			return nil, fmt.Errorf("%w: kline %d: %v", ErrBadResponse, i, err)
		}
		out = append(out, k)
	}
	return out, nil
}

// parseKline reads: open time, open, high, low, close, volume, close time,
// quote volume, trades, taker buy base, taker buy quote, ignore.
func parseKline(row []json.RawMessage) (exchange.Kline, error) {
	if len(row) < 9 {
		return exchange.Kline{}, fmt.Errorf("want at least 9 fields, got %d", len(row))
	}
	var k exchange.Kline
	ms := func(i int) (time.Time, error) {
		var v int64
		if err := json.Unmarshal(row[i], &v); err != nil {
			return time.Time{}, fmt.Errorf("field %d: %w", i, err)
		}
		return time.UnixMilli(v), nil
	}
	dec := func(i int) (*big.Rat, error) {
		var s string
		if err := json.Unmarshal(row[i], &s); err != nil {
			return nil, fmt.Errorf("field %d: %w", i, err)
		}
		return exchange.ParseDecimal(s)
	}
	var err error
	if k.OpenTime, err = ms(0); err != nil {
		return k, err
	}
	if k.CloseTime, err = ms(6); err != nil {
		return k, err
	}
	if !k.CloseTime.After(k.OpenTime) {
		return k, fmt.Errorf("close time not after open time")
	}
	for i, dst := range []**big.Rat{&k.Open, &k.High, &k.Low, &k.Close, &k.Volume} {
		if *dst, err = dec(i + 1); err != nil {
			return k, err
		}
	}
	if k.QuoteVol, err = dec(7); err != nil {
		return k, err
	}
	if err := json.Unmarshal(row[8], &k.Trades); err != nil {
		return k, fmt.Errorf("field 8: %w", err)
	}
	return k, nil
}

// Account returns account flags, commission rates and non-zero balances (signed, weight 20).
func (c *Client) Account(ctx context.Context) (exchange.AccountInfo, error) {
	var w struct {
		MakerCommission json.Number `json:"makerCommission"`
		TakerCommission json.Number `json:"takerCommission"`
		CommissionRates *struct {
			Maker string `json:"maker"`
			Taker string `json:"taker"`
		} `json:"commissionRates"`
		CanTrade    *bool `json:"canTrade"`
		CanWithdraw *bool `json:"canWithdraw"`
		CanDeposit  *bool `json:"canDeposit"`
		Balances    []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	q := url.Values{"omitZeroBalances": {"true"}}
	if err := c.get(ctx, request{host: c.base, path: "/api/v3/account", query: q, signed: true, weight: 20}, &w); err != nil {
		return exchange.AccountInfo{}, err
	}
	if w.CanTrade == nil || w.CanWithdraw == nil || w.CanDeposit == nil || w.CommissionRates == nil || w.Balances == nil {
		return exchange.AccountInfo{}, fmt.Errorf("%w: account lacks flags, commissionRates or balances", ErrBadResponse)
	}
	info := exchange.AccountInfo{
		CanTrade: *w.CanTrade, CanWithdraw: *w.CanWithdraw, CanDeposit: *w.CanDeposit,
		RawMakerCommission: w.MakerCommission.String(), RawTakerCommission: w.TakerCommission.String(),
	}
	var err error
	if info.MakerRate, err = exchange.ParseDecimal(w.CommissionRates.Maker); err != nil {
		return exchange.AccountInfo{}, fmt.Errorf("%w: commissionRates.maker: %v", ErrBadResponse, err)
	}
	if info.TakerRate, err = exchange.ParseDecimal(w.CommissionRates.Taker); err != nil {
		return exchange.AccountInfo{}, fmt.Errorf("%w: commissionRates.taker: %v", ErrBadResponse, err)
	}
	for _, b := range w.Balances {
		if b.Asset == "" {
			return exchange.AccountInfo{}, fmt.Errorf("%w: balance without asset", ErrBadResponse)
		}
		free, err := exchange.ParseDecimal(b.Free)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("%w: balance %s: %v", ErrBadResponse, b.Asset, err)
		}
		locked, err := exchange.ParseDecimal(b.Locked)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("%w: balance %s: %v", ErrBadResponse, b.Asset, err)
		}
		info.Balances = append(info.Balances, exchange.Balance{Asset: b.Asset, Free: free, Locked: locked})
	}
	return info, nil
}

// requiredKeyFlags are the apiRestrictions fields the safety checks depend
// on. A response without any of them is rejected rather than read as false.
var requiredKeyFlags = [...]string{"ipRestrict", "enableReading", "enableSpotAndMarginTrading", "enableWithdrawals"}

// KeyPermissions returns what the API key may do (signed, GET /sapi/v1/account/apiRestrictions).
// Every other boolean field that is true is listed in OtherEnabled, so a
// newly added permission is visible instead of silently ignored.
func (c *Client) KeyPermissions(ctx context.Context) (exchange.KeyPermissions, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, request{host: c.base, path: "/sapi/v1/account/apiRestrictions", signed: true, weight: 1}, &raw); err != nil {
		return exchange.KeyPermissions{}, err
	}
	flags := make(map[string]bool, len(requiredKeyFlags))
	for _, name := range requiredKeyFlags {
		v, ok := raw[name]
		if !ok {
			return exchange.KeyPermissions{}, fmt.Errorf("%w: apiRestrictions lacks %s", ErrBadResponse, name)
		}
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return exchange.KeyPermissions{}, fmt.Errorf("%w: apiRestrictions %s is not a boolean", ErrBadResponse, name)
		}
		flags[name] = b
	}
	p := exchange.KeyPermissions{
		IPRestricted: flags["ipRestrict"],
		Reading:      flags["enableReading"],
		SpotTrading:  flags["enableSpotAndMarginTrading"],
		Withdrawals:  flags["enableWithdrawals"],
	}
	for k, v := range raw {
		if slices.Contains(requiredKeyFlags[:], k) {
			continue
		}
		var b bool
		if json.Unmarshal(v, &b) == nil && b {
			p.OtherEnabled = append(p.OtherEnabled, k)
		}
	}
	slices.Sort(p.OtherEnabled)
	return p, nil
}
