package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

// ServerTime returns the exchange clock.
func (c *Client) ServerTime(ctx context.Context) (time.Time, error) {
	var v struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/time"}, &v); err != nil {
		return time.Time{}, err
	}
	if v.ServerTime == 0 {
		return time.Time{}, fmt.Errorf("binance: serverTime missing")
	}
	return time.UnixMilli(v.ServerTime), nil
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
	ApplyToMarket    bool   `json:"applyToMarket"`    // MIN_NOTIONAL
	ApplyMinToMarket bool   `json:"applyMinToMarket"` // NOTIONAL
}

// Symbols returns spot symbols with their filters. With names empty it
// returns every symbol (request weight 20).
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
		Symbols []struct {
			Symbol      string       `json:"symbol"`
			Status      string       `json:"status"`
			BaseAsset   string       `json:"baseAsset"`
			QuoteAsset  string       `json:"quoteAsset"`
			OrderTypes  []string     `json:"orderTypes"`
			OCOAllowed  bool         `json:"ocoAllowed"`
			SpotAllowed bool         `json:"isSpotTradingAllowed"`
			DefaultSTP  string       `json:"defaultSelfTradePreventionMode"`
			AllowedSTP  []string     `json:"allowedSelfTradePreventionModes"`
			Filters     []wireFilter `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/exchangeInfo", query: q}, &v); err != nil {
		return nil, err
	}
	out := make([]exchange.Symbol, 0, len(v.Symbols))
	for _, w := range v.Symbols {
		f, err := parseFilters(w.Filters)
		if err != nil {
			return nil, fmt.Errorf("binance: symbol %s: %w", w.Symbol, err)
		}
		out = append(out, exchange.Symbol{
			Name: w.Symbol, Base: w.BaseAsset, Quote: w.QuoteAsset, Status: w.Status,
			SpotEnabled: w.Status == "TRADING" && w.SpotAllowed, OCOEnabled: w.OCOAllowed,
			OrderTypes: w.OrderTypes, STPDefault: w.DefaultSTP, STPAllowed: w.AllowedSTP, Filters: f,
		})
	}
	return out, nil
}

func parseFilters(ws []wireFilter) (exchange.Filters, error) {
	var f exchange.Filters
	set := func(s string, dst **big.Rat) error {
		if s == "" {
			return nil
		}
		r, err := exchange.ParseDecimal(s)
		if err != nil {
			return err
		}
		*dst = r
		return nil
	}
	for _, w := range ws {
		var errs []error
		switch w.FilterType {
		case "PRICE_FILTER":
			errs = append(errs, set(w.TickSize, &f.TickSize), set(w.MinPrice, &f.MinPrice), set(w.MaxPrice, &f.MaxPrice))
		case "LOT_SIZE":
			errs = append(errs, set(w.StepSize, &f.StepSize), set(w.MinQty, &f.MinQty), set(w.MaxQty, &f.MaxQty))
		case "NOTIONAL":
			errs = append(errs, set(w.MinNotional, &f.MinNotional))
			f.NotionalAppliesToMarket = w.ApplyMinToMarket
		case "MIN_NOTIONAL":
			if f.MinNotional == nil { // NOTIONAL takes precedence when both exist
				errs = append(errs, set(w.MinNotional, &f.MinNotional))
				f.NotionalAppliesToMarket = w.ApplyToMarket
			}
		}
		for _, e := range errs {
			if e != nil {
				return f, fmt.Errorf("filter %s: %w", w.FilterType, e)
			}
		}
	}
	return f, nil
}

// Tickers24h returns the rolling 24h summary of every symbol (request weight 80).
func (c *Client) Tickers24h(ctx context.Context) ([]exchange.Ticker, error) {
	var v []struct {
		Symbol      string `json:"symbol"`
		LastPrice   string `json:"lastPrice"`
		QuoteVolume string `json:"quoteVolume"`
	}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/ticker/24hr", query: url.Values{"type": {"MINI"}}}, &v); err != nil {
		return nil, err
	}
	out := make([]exchange.Ticker, 0, len(v))
	for _, w := range v {
		lp, err := exchange.ParseDecimal(w.LastPrice)
		if err != nil {
			return nil, fmt.Errorf("binance: ticker %s: %w", w.Symbol, err)
		}
		qv, err := exchange.ParseDecimal(w.QuoteVolume)
		if err != nil {
			return nil, fmt.Errorf("binance: ticker %s: %w", w.Symbol, err)
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
		LastUpdateID int64       `json:"lastUpdateId"`
		Bids         [][2]string `json:"bids"`
		Asks         [][2]string `json:"asks"`
	}
	q := url.Values{"symbol": {s.Name}, "limit": {strconv.Itoa(limit)}}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/depth", query: q}, &w); err != nil {
		return exchange.OrderBook{}, err
	}
	ob := exchange.OrderBook{LastUpdateID: w.LastUpdateID}
	var err error
	if ob.Bids, err = levels(w.Bids); err != nil {
		return exchange.OrderBook{}, err
	}
	if ob.Asks, err = levels(w.Asks); err != nil {
		return exchange.OrderBook{}, err
	}
	return ob, nil
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

// Klines returns up to limit candles (1..1000), oldest first. The newest one
// is usually still open.
func (c *Client) Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("binance: klines limit %d not in [1, 1000]", limit)
	}
	if interval == "" {
		return nil, fmt.Errorf("binance: klines interval required")
	}
	var rows [][]json.RawMessage
	q := url.Values{"symbol": {s.Name}, "interval": {interval}, "limit": {strconv.Itoa(limit)}}
	if err := c.get(ctx, request{host: c.market, path: "/api/v3/klines", query: q}, &rows); err != nil {
		return nil, err
	}
	out := make([]exchange.Kline, 0, len(rows))
	for i, row := range rows {
		k, err := parseKline(row)
		if err != nil {
			return nil, fmt.Errorf("binance: kline %d: %w", i, err)
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
		CommissionRates struct {
			Maker string `json:"maker"`
			Taker string `json:"taker"`
		} `json:"commissionRates"`
		CanTrade    bool `json:"canTrade"`
		CanWithdraw bool `json:"canWithdraw"`
		CanDeposit  bool `json:"canDeposit"`
		Balances    []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	q := url.Values{"omitZeroBalances": {"true"}}
	if err := c.get(ctx, request{host: c.base, path: "/api/v3/account", query: q, signed: true}, &w); err != nil {
		return exchange.AccountInfo{}, err
	}
	info := exchange.AccountInfo{
		CanTrade: w.CanTrade, CanWithdraw: w.CanWithdraw, CanDeposit: w.CanDeposit,
		RawMakerCommission: w.MakerCommission.String(), RawTakerCommission: w.TakerCommission.String(),
	}
	for _, p := range []struct {
		s   string
		dst **big.Rat
	}{{w.CommissionRates.Maker, &info.MakerRate}, {w.CommissionRates.Taker, &info.TakerRate}} {
		if p.s == "" {
			continue
		}
		r, err := exchange.ParseDecimal(p.s)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binance: commission rate: %w", err)
		}
		*p.dst = r
	}
	for _, b := range w.Balances {
		free, err := exchange.ParseDecimal(b.Free)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binance: balance %s: %w", b.Asset, err)
		}
		locked, err := exchange.ParseDecimal(b.Locked)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binance: balance %s: %w", b.Asset, err)
		}
		info.Balances = append(info.Balances, exchange.Balance{Asset: b.Asset, Free: free, Locked: locked})
	}
	return info, nil
}

// KeyPermissions returns what the API key may do (signed, GET /sapi/v1/account/apiRestrictions).
func (c *Client) KeyPermissions(ctx context.Context) (exchange.KeyPermissions, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, request{host: c.base, path: "/sapi/v1/account/apiRestrictions", signed: true}, &raw); err != nil {
		return exchange.KeyPermissions{}, err
	}
	flag := func(name string) bool {
		var b bool
		if v, ok := raw[name]; ok && json.Unmarshal(v, &b) == nil {
			return b
		}
		return false
	}
	p := exchange.KeyPermissions{
		IPRestricted: flag("ipRestrict"),
		Reading:      flag("enableReading"),
		SpotTrading:  flag("enableSpotAndMarginTrading"),
		Withdrawals:  flag("enableWithdrawals"),
	}
	known := map[string]bool{"ipRestrict": true, "enableReading": true, "enableSpotAndMarginTrading": true, "enableWithdrawals": true}
	for k := range raw {
		if known[k] || !(strings.HasPrefix(k, "enable") || strings.HasPrefix(k, "permits")) {
			continue
		}
		if flag(k) {
			p.OtherEnabled = append(p.OtherEnabled, k)
		}
	}
	sort.Strings(p.OtherEnabled)
	return p, nil
}
