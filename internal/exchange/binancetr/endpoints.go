package binancetr

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

// ServerTime returns the exchange clock.
func (c *Client) ServerTime(ctx context.Context) (time.Time, error) {
	body, err := c.do(ctx, request{method: http.MethodGet, host: c.base, path: "/open/v1/common/time"})
	if err != nil {
		return time.Time{}, err
	}
	data, env, shape, err := unwrap(body)
	if err != nil {
		return time.Time{}, err
	}
	if shape == ShapeEnvelope && env.Timestamp > 0 {
		return time.UnixMilli(env.Timestamp), nil
	}
	// Raw Binance-global style: {"serverTime": ...}
	var raw struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.ServerTime == 0 {
		return time.Time{}, fmt.Errorf("binancetr: server time missing from response")
	}
	return time.UnixMilli(raw.ServerTime), nil
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
	ApplyToMarket    bool   `json:"applyToMarket"`    // MIN_NOTIONAL; also on NOTIONAL in Binance TR docs
	ApplyMinToMarket *bool  `json:"applyMinToMarket"` // NOTIONAL (Binance global form)
	ApplyMaxToMarket bool   `json:"applyMaxToMarket"` // NOTIONAL
}

type wireSymbol struct {
	Type              int          `json:"type"`
	Symbol            string       `json:"symbol"`
	BaseAsset         string       `json:"baseAsset"`
	QuoteAsset        string       `json:"quoteAsset"`
	Filters           []wireFilter `json:"filters"`
	OrderTypes        []string     `json:"orderTypes"`
	DefaultSTP        string       `json:"defaultSelfTradePreventionMode"`
	AllowedSTP        []string     `json:"allowedSelfTradePreventionModes"`
	OCOEnable         int          `json:"ocoEnable"`
	SpotTradingEnable int          `json:"spotTradingEnable"`
}

// Symbols returns every symbol with its filters.
func (c *Client) Symbols(ctx context.Context) ([]exchange.Symbol, error) {
	body, err := c.do(ctx, request{method: http.MethodGet, host: c.base, path: "/open/v1/common/symbols"})
	if err != nil {
		return nil, err
	}
	data, _, _, err := unwrap(body)
	if err != nil {
		return nil, err
	}
	var payload struct {
		List []wireSymbol `json:"list"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("binancetr: decode symbols: %w", err)
	}
	out := make([]exchange.Symbol, 0, len(payload.List))
	for _, w := range payload.List {
		f, err := parseFilters(w.Filters)
		if err != nil {
			return nil, fmt.Errorf("binancetr: symbol %s: %w", w.Symbol, err)
		}
		out = append(out, exchange.Symbol{
			Name: w.Symbol, Base: w.BaseAsset, Quote: w.QuoteAsset, Type: w.Type,
			SpotEnabled: w.SpotTradingEnable == 1, OCOEnabled: w.OCOEnable == 1,
			OrderTypes: w.OrderTypes, STPDefault: w.DefaultSTP, STPAllowed: w.AllowedSTP,
			Filters: f,
		})
	}
	return out, nil
}

func parseFilters(ws []wireFilter) (exchange.Filters, error) {
	var f exchange.Filters
	dec := func(s string, dst **big.Rat) error {
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
	var sawNotional bool
	for _, w := range ws {
		var errs []error
		switch w.FilterType {
		case "PRICE_FILTER":
			errs = append(errs, dec(w.TickSize, &f.TickSize), dec(w.MinPrice, &f.MinPrice), dec(w.MaxPrice, &f.MaxPrice))
		case "LOT_SIZE":
			errs = append(errs, dec(w.StepSize, &f.StepSize), dec(w.MinQty, &f.MinQty), dec(w.MaxQty, &f.MaxQty))
		case "NOTIONAL":
			// NOTIONAL takes precedence over MIN_NOTIONAL whatever the order.
			// Binance TR's documented NOTIONAL example carries applyToMarket;
			// the Binance global form is applyMinToMarket. Prefer the latter
			// when present.
			sawNotional = true
			f.MinNotional, f.MaxNotional = nil, nil
			errs = append(errs, dec(w.MinNotional, &f.MinNotional), dec(w.MaxNotional, &f.MaxNotional))
			f.NotionalAppliesToMarket = w.ApplyToMarket
			if w.ApplyMinToMarket != nil {
				f.NotionalAppliesToMarket = *w.ApplyMinToMarket
			}
			f.MaxNotionalAppliesToMarket = w.ApplyMaxToMarket
		case "MIN_NOTIONAL":
			if sawNotional {
				continue
			}
			errs = append(errs, dec(w.MinNotional, &f.MinNotional))
			f.NotionalAppliesToMarket = w.ApplyToMarket
		}
		for _, e := range errs {
			if e != nil {
				return f, fmt.Errorf("filter %s: %w", w.FilterType, e)
			}
		}
	}
	return f, nil
}

// marketHost routes market data by symbol type and formats the symbol name.
// Type 1 symbols drop the underscore; others keep the venue name (the format
// for non-main hosts is not documented and is verified by venuecheck).
func (c *Client) marketHost(s exchange.Symbol) (*url.URL, string, string) {
	if s.Type == 1 {
		return c.mainMarket, strings.ReplaceAll(s.Name, "_", ""), "/api/v3/depth"
	}
	return c.nextMarket, s.Name, "/api/v1/depth"
}

var validDepthLimits = map[int]bool{5: true, 10: true, 20: true, 50: true, 100: true, 500: true}

// Depth returns an order book snapshot and the response shape that was received.
func (c *Client) Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, Shape, error) {
	if !validDepthLimits[limit] {
		return exchange.OrderBook{}, "", fmt.Errorf("binancetr: depth limit %d not one of 5,10,20,50,100,500", limit)
	}
	host, name, path := c.marketHost(s)
	q := url.Values{"symbol": {name}, "limit": {strconv.Itoa(limit)}}
	body, err := c.do(ctx, request{method: http.MethodGet, host: host, path: path, query: q})
	if err != nil {
		return exchange.OrderBook{}, "", err
	}
	data, _, shape, err := unwrap(body)
	if err != nil {
		return exchange.OrderBook{}, shape, err
	}
	var w struct {
		LastUpdateID int64       `json:"lastUpdateId"`
		Bids         [][2]string `json:"bids"`
		Asks         [][2]string `json:"asks"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return exchange.OrderBook{}, shape, fmt.Errorf("binancetr: decode depth: %w", err)
	}
	ob := exchange.OrderBook{LastUpdateID: w.LastUpdateID}
	if ob.Bids, err = levels(w.Bids); err != nil {
		return exchange.OrderBook{}, shape, err
	}
	if ob.Asks, err = levels(w.Asks); err != nil {
		return exchange.OrderBook{}, shape, err
	}
	return ob, shape, nil
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

// Klines returns up to limit candles (1..1000), oldest first, ending at the most recent.
func (c *Client) Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, Shape, error) {
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("binancetr: klines limit %d not in [1, 1000]", limit)
	}
	if interval == "" {
		return nil, "", fmt.Errorf("binancetr: klines interval required")
	}
	host, name, _ := c.marketHost(s)
	q := url.Values{"symbol": {name}, "interval": {interval}, "limit": {strconv.Itoa(limit)}}
	body, err := c.do(ctx, request{method: http.MethodGet, host: host, path: "/api/v1/klines", query: q})
	if err != nil {
		return nil, "", err
	}
	data, _, shape, err := unwrap(body)
	if err != nil {
		return nil, shape, err
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, shape, fmt.Errorf("binancetr: decode klines: %w", err)
	}
	out := make([]exchange.Kline, 0, len(rows))
	for i, row := range rows {
		k, err := parseKline(row)
		if err != nil {
			return nil, shape, fmt.Errorf("binancetr: kline %d: %w", i, err)
		}
		out = append(out, k)
	}
	return out, shape, nil
}

// parseKline reads the documented 12-field array: open time, open, high, low,
// close, volume, close time, quote volume, trades, taker buy base, taker buy quote, ignore.
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

// Account returns account flags, fiat (TRY) commission rates and balances (signed).
func (c *Client) Account(ctx context.Context) (exchange.AccountInfo, error) {
	body, err := c.do(ctx, request{method: http.MethodGet, host: c.base, path: "/open/v1/account/spot", signed: true})
	if err != nil {
		return exchange.AccountInfo{}, err
	}
	data, _, _, err := unwrap(body)
	if err != nil {
		return exchange.AccountInfo{}, err
	}
	var w struct {
		MakerCommission     string `json:"makerCommission"`
		TakerCommission     string `json:"takerCommission"`
		FiatMakerCommission string `json:"fiatMakerCommission"`
		FiatTakerCommission string `json:"fiatTakerCommission"`
		CanTrade            int    `json:"canTrade"`
		CanWithdraw         int    `json:"canWithdraw"`
		CanDeposit          int    `json:"canDeposit"`
		Assets              []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"accountAssets"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return exchange.AccountInfo{}, fmt.Errorf("binancetr: decode account: %w", err)
	}
	info := exchange.AccountInfo{
		CanTrade: w.CanTrade == 1, CanWithdraw: w.CanWithdraw == 1, CanDeposit: w.CanDeposit == 1,
		RawMakerCommission: w.MakerCommission, RawTakerCommission: w.TakerCommission,
	}
	for _, p := range []struct {
		s   string
		dst **big.Rat
	}{{w.FiatMakerCommission, &info.MakerRate}, {w.FiatTakerCommission, &info.TakerRate}} {
		if p.s == "" {
			continue
		}
		r, err := exchange.ParseDecimal(p.s)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binancetr: account commission: %w", err)
		}
		*p.dst = r
	}
	for _, a := range w.Assets {
		free, err := exchange.ParseDecimal(a.Free)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binancetr: balance %s: %w", a.Asset, err)
		}
		locked, err := exchange.ParseDecimal(a.Locked)
		if err != nil {
			return exchange.AccountInfo{}, fmt.Errorf("binancetr: balance %s: %w", a.Asset, err)
		}
		info.Balances = append(info.Balances, exchange.Balance{Asset: a.Asset, Free: free, Locked: locked})
	}
	return info, nil
}
