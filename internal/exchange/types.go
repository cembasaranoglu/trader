// Package exchange holds venue-neutral market types shared by exchange
// adapters and their consumers. Prices and quantities are exact decimals
// (*big.Rat); they are never stored as float64.
package exchange

import (
	"fmt"
	"math/big"
	"time"
)

// Symbol describes one tradable pair and its exchange rules.
type Symbol struct {
	Name        string // venue-native name, e.g. "BTC_TRY"
	Base        string
	Quote       string
	Type        int  // venue routing class (Binance TR: 1 main, 2/3 next)
	SpotEnabled bool // spot trading allowed right now
	OCOEnabled  bool
	OrderTypes  []string
	STPDefault  string
	STPAllowed  []string
	Filters     Filters
}

// Filters are the exchange-side order rules for a symbol. A nil field means
// the exchange did not publish that filter.
type Filters struct {
	TickSize    *big.Rat
	MinPrice    *big.Rat
	MaxPrice    *big.Rat
	StepSize    *big.Rat
	MinQty      *big.Rat
	MaxQty      *big.Rat
	MinNotional *big.Rat
	// NotionalAppliesToMarket reports whether MinNotional also applies to MARKET orders.
	NotionalAppliesToMarket bool
}

// Level is one price level of an order book.
type Level struct {
	Price *big.Rat
	Qty   *big.Rat
}

// OrderBook is a depth snapshot. Bids are best (highest) first, asks best (lowest) first.
type OrderBook struct {
	LastUpdateID int64
	Bids         []Level
	Asks         []Level
}

// Kline is one candlestick.
type Kline struct {
	OpenTime  time.Time
	CloseTime time.Time
	Open      *big.Rat
	High      *big.Rat
	Low       *big.Rat
	Close     *big.Rat
	Volume    *big.Rat // base asset volume
	QuoteVol  *big.Rat
	Trades    int64
}

// AccountInfo is the account-level state returned by a venue.
type AccountInfo struct {
	CanTrade    bool
	CanWithdraw bool // account-level flag; it does not describe API key permissions
	CanDeposit  bool
	// Fiat (TRY) pair commission rates as fractions (0.0015 = 0.15%).
	// Nil when not published.
	FiatMakerRate *big.Rat
	FiatTakerRate *big.Rat
	// Raw crypto-pair commission values as published. Their unit is not
	// documented by Binance TR, so they are not converted.
	RawMakerCommission string
	RawTakerCommission string
	Balances           []Balance
}

// Balance is the free and locked amount of one asset.
type Balance struct {
	Asset  string
	Free   *big.Rat
	Locked *big.Rat
}

// ParseDecimal parses a decimal string such as "0.00000100" exactly.
func ParseDecimal(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("exchange: invalid decimal %q", s)
	}
	return r, nil
}
