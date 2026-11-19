package marketdata

import (
	"context"
	"testing"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance/ws"
)

type nopSink struct{}

func (nopSink) OnBar(Bar)       {}
func (nopSink) OnTrade(Trade)   {}
func (nopSink) OnTicker(Ticker) {}

func benchManager(b *testing.B) *Manager {
	b.Helper()
	m, err := New(context.Background(), Config{
		Symbols:    []exchange.Symbol{{Name: "BTCUSDT", Filters: exchange.Filters{TickSize: rat("0.01"), StepSize: rat("0.00001")}}},
		Intervals:  []string{"1m"},
		DepthLimit: 1000,
		MaxDataAge: 5e9,
	}, &fakeREST{}, nopSink{})
	if err != nil {
		b.Fatal(err)
	}
	return m
}

func BenchmarkHandleAggTrade(b *testing.B) {
	m := benchManager(b)
	ev := &ws.AggTradeEvent{Symbol: "BTCUSDT", Price: "90500.01", Qty: "0.00123", FirstTradeID: 1, LastTradeID: 2}
	msg := ws.Message{Stream: "btcusdt@aggTrade", Event: ev}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ev.AggID = int64(i + 1)
		m.Handle(msg)
	}
}

func BenchmarkHandleBookTicker(b *testing.B) {
	m := benchManager(b)
	ev := &ws.BookTickerEvent{Symbol: "BTCUSDT", BidPrice: "90500.01", BidQty: "1.5", AskPrice: "90500.02", AskQty: "0.3"}
	msg := ws.Message{Stream: "btcusdt@bookTicker", Event: ev}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ev.UpdateID = int64(i + 1)
		m.Handle(msg)
	}
}
