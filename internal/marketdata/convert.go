package marketdata

import (
	"fmt"
	"math/big"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
)

// NextOpen returns the open time of the kline after the one opening at t.
// Binance kline intervals are UTC; 1M follows calendar months.
func NextOpen(interval string, t time.Time) (time.Time, error) {
	t = t.UTC()
	switch interval {
	case "1M":
		return t.AddDate(0, 1, 0), nil
	case "1s":
		return t.Add(time.Second), nil
	case "1m", "3m", "5m", "15m", "30m":
		return t.Add(mustMinutes(interval)), nil
	case "1h", "2h", "4h", "6h", "8h", "12h":
		d, _ := time.ParseDuration(interval)
		return t.Add(d), nil
	case "1d":
		return t.Add(24 * time.Hour), nil
	case "3d":
		return t.Add(72 * time.Hour), nil
	case "1w":
		return t.Add(7 * 24 * time.Hour), nil
	}
	return time.Time{}, fmt.Errorf("%w: %q", ErrUnknownInterval, interval)
}

func mustMinutes(iv string) time.Duration {
	d, err := time.ParseDuration(iv)
	if err != nil {
		panic(err)
	}
	return d
}

func decs(vals ...string) ([]*big.Rat, error) {
	out := make([]*big.Rat, len(vals))
	for i, v := range vals {
		r, err := exchange.ParseDecimal(v)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func barFromEvent(ev *ws.KlineEvent) (Bar, error) {
	r, err := decs(ev.Open, ev.High, ev.Low, ev.Close, ev.Volume, ev.QuoteVolume)
	if err != nil {
		return Bar{}, err
	}
	return Bar{Symbol: ev.Symbol, Interval: ev.Interval, OpenTime: ev.OpenTime, CloseTime: ev.CloseTime,
		Open: r[0], High: r[1], Low: r[2], Close: r[3], Volume: r[4], QuoteVolume: r[5], Trades: ev.Trades}, nil
}

func barFromKline(symbol, interval string, k exchange.Kline) Bar {
	return Bar{Symbol: symbol, Interval: interval, OpenTime: k.OpenTime.UTC(), CloseTime: k.CloseTime.UTC(),
		Open: k.Open, High: k.High, Low: k.Low, Close: k.Close, Volume: k.Volume, QuoteVolume: k.QuoteVol, Trades: k.Trades}
}

func tradeFrom(ev *ws.AggTradeEvent) (Trade, error) {
	r, err := decs(ev.Price, ev.Qty)
	if err != nil {
		return Trade{}, err
	}
	return Trade{Symbol: ev.Symbol, AggID: ev.AggID, FirstTradeID: ev.FirstTradeID, LastTradeID: ev.LastTradeID,
		Price: r[0], Qty: r[1], Time: ev.TradeTime, BuyerMaker: ev.BuyerMaker}, nil
}

func tickerFrom(ev *ws.BookTickerEvent) (Ticker, error) {
	r, err := decs(ev.BidPrice, ev.BidQty, ev.AskPrice, ev.AskQty)
	if err != nil {
		return Ticker{}, err
	}
	return Ticker{Symbol: ev.Symbol, UpdateID: ev.UpdateID, BidPrice: r[0], BidQty: r[1], AskPrice: r[2], AskQty: r[3]}, nil
}
