package ws

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrMalformed reports a message that does not match the documented format.
var ErrMalformed = errors.New("ws: malformed message")

// Message is one decoded combined-stream message.
type Message struct {
	Stream   string    // stream name, e.g. "btcusdt@aggTrade"
	Event    any       // one of the *Event types below
	ConnID   uint64    // connection that delivered it (duplicates are possible during rotation)
	Received time.Time // local receive time
}

// KlineEvent is a <symbol>@kline_<interval> update. Prices are the exchange's
// decimal strings; consumers convert them.
type KlineEvent struct {
	EventTime   time.Time
	Symbol      string
	Interval    string
	OpenTime    time.Time
	CloseTime   time.Time
	Open        string
	High        string
	Low         string
	Close       string
	Volume      string
	QuoteVolume string
	Trades      int64
	Closed      bool
}

// BookTickerEvent is a <symbol>@bookTicker update.
type BookTickerEvent struct {
	UpdateID int64
	Symbol   string
	BidPrice string
	BidQty   string
	AskPrice string
	AskQty   string
}

// DepthEvent is a <symbol>@depth or <symbol>@depth@100ms diff update.
type DepthEvent struct {
	EventTime     time.Time
	Symbol        string
	FirstUpdateID int64 // U
	FinalUpdateID int64 // u
	Bids          [][2]string
	Asks          [][2]string
}

// AggTradeEvent is a <symbol>@aggTrade update.
type AggTradeEvent struct {
	EventTime    time.Time
	Symbol       string
	AggID        int64
	Price        string
	Qty          string
	FirstTradeID int64
	LastTradeID  int64
	TradeTime    time.Time
	BuyerMaker   bool
}

// ServerShutdownEvent announces an upcoming server shutdown; the connection
// should be replaced as soon as possible.
type ServerShutdownEvent struct {
	EventTime    time.Time
	ShutdownTime time.Time
}

// Decode parses one combined-stream message: {"stream": ..., "data": ...}.
// Every documented field the system uses is required.
//
// encoding/json matches object keys case-insensitively when no field has the
// exact name. Binance payloads use keys that differ only in case ("m"/"M",
// "l"/"L", "v"/"V", "q"/"Q"), so every documented key is declared explicitly,
// including ignored ones; otherwise an ignored key would overwrite a used one.
func Decode(b []byte) (string, any, error) {
	var env struct {
		Stream *string          `json:"stream"`
		Data   *json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return "", nil, fmt.Errorf("%w: envelope: %v", ErrMalformed, err)
	}
	if env.Stream == nil || env.Data == nil || *env.Stream == "" || bytes.Equal(bytes.TrimSpace(*env.Data), []byte("null")) {
		return "", nil, fmt.Errorf("%w: envelope lacks stream or data", ErrMalformed)
	}
	stream, data := *env.Stream, []byte(*env.Data)
	var (
		ev  any
		err error
	)
	switch {
	case stream == "!serverShutdown":
		ev, err = decodeShutdown(data)
	case strings.HasSuffix(stream, "@aggTrade"):
		ev, err = decodeAggTrade(data)
	case strings.HasSuffix(stream, "@bookTicker"):
		ev, err = decodeBookTicker(data)
	case strings.HasSuffix(stream, "@depth"), strings.HasSuffix(stream, "@depth@100ms"):
		ev, err = decodeDepth(data)
	case strings.Contains(stream, "@kline_"):
		ev, err = decodeKline(data)
	default:
		return stream, nil, fmt.Errorf("%w: unsupported stream %q", ErrMalformed, clip(stream))
	}
	if err != nil {
		return stream, nil, fmt.Errorf("%w: %s: %v", ErrMalformed, clip(stream), err)
	}
	if err := matchStream(stream, ev); err != nil {
		return stream, nil, fmt.Errorf("%w: %s: %v", ErrMalformed, clip(stream), err)
	}
	return stream, ev, nil
}

// matchStream checks that the payload belongs to the stream it arrived on:
// the stream's symbol is the lower-case payload symbol, and a kline's
// interval matches the stream's.
func matchStream(stream string, ev any) error {
	sym, kind, _ := strings.Cut(stream, "@")
	var payloadSym string
	switch e := ev.(type) {
	case *ServerShutdownEvent:
		return nil
	case *AggTradeEvent:
		payloadSym = e.Symbol
	case *BookTickerEvent:
		payloadSym = e.Symbol
	case *DepthEvent:
		payloadSym = e.Symbol
	case *KlineEvent:
		payloadSym = e.Symbol
		if kind != "kline_"+e.Interval {
			return fmt.Errorf("kline interval %q does not match the stream", clip(e.Interval))
		}
	}
	if sym != strings.ToLower(payloadSym) {
		return fmt.Errorf("payload symbol %q does not match the stream", clip(payloadSym))
	}
	return nil
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

func ms(v *int64, name string) (time.Time, error) {
	if v == nil || *v <= 0 {
		return time.Time{}, fmt.Errorf("%s missing", name)
	}
	return time.UnixMilli(*v).UTC(), nil
}

func req(s *string, name string) (string, error) {
	if s == nil || *s == "" {
		return "", fmt.Errorf("%s missing", name)
	}
	return *s, nil
}

func decodeShutdown(b []byte) (*ServerShutdownEvent, error) {
	var w struct {
		E  *string `json:"e"`
		ET *int64  `json:"E"`
		T  *int64  `json:"T"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.E == nil || *w.E != "serverShutdown" {
		return nil, errors.New("event type is not serverShutdown")
	}
	et, err := ms(w.ET, "E")
	if err != nil {
		return nil, err
	}
	// T (shutdown time) was added later; tolerate its absence.
	ev := &ServerShutdownEvent{EventTime: et}
	if w.T != nil && *w.T > 0 {
		ev.ShutdownTime = time.UnixMilli(*w.T).UTC()
	}
	return ev, nil
}

func decodeAggTrade(b []byte) (*AggTradeEvent, error) {
	var w struct {
		E  *string         `json:"e"`
		ET *int64          `json:"E"`
		S  *string         `json:"s"`
		A  *int64          `json:"a"`
		P  *string         `json:"p"`
		Q  *string         `json:"q"`
		F  *int64          `json:"f"`
		L  *int64          `json:"l"`
		T  *int64          `json:"T"`
		M  *bool           `json:"m"`
		MI json.RawMessage `json:"M"` // "Ignore"; declared so it cannot match "m"
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.E == nil || *w.E != "aggTrade" || w.A == nil || w.F == nil || w.L == nil || w.M == nil {
		return nil, errors.New("aggTrade lacks required fields")
	}
	ev := &AggTradeEvent{AggID: *w.A, FirstTradeID: *w.F, LastTradeID: *w.L, BuyerMaker: *w.M}
	var err error
	if ev.EventTime, err = ms(w.ET, "E"); err != nil {
		return nil, err
	}
	if ev.TradeTime, err = ms(w.T, "T"); err != nil {
		return nil, err
	}
	if ev.Symbol, err = req(w.S, "s"); err != nil {
		return nil, err
	}
	if ev.Price, err = req(w.P, "p"); err != nil {
		return nil, err
	}
	if ev.Qty, err = req(w.Q, "q"); err != nil {
		return nil, err
	}
	return ev, nil
}

func decodeBookTicker(b []byte) (*BookTickerEvent, error) {
	var w struct {
		U  *int64  `json:"u"`
		S  *string `json:"s"`
		B  *string `json:"b"`
		BQ *string `json:"B"`
		A  *string `json:"a"`
		AQ *string `json:"A"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.U == nil {
		return nil, errors.New("u missing")
	}
	ev := &BookTickerEvent{UpdateID: *w.U}
	var err error
	for _, f := range []struct {
		src  *string
		dst  *string
		name string
	}{{w.S, &ev.Symbol, "s"}, {w.B, &ev.BidPrice, "b"}, {w.BQ, &ev.BidQty, "B"}, {w.A, &ev.AskPrice, "a"}, {w.AQ, &ev.AskQty, "A"}} {
		if *f.dst, err = req(f.src, f.name); err != nil {
			return nil, err
		}
	}
	return ev, nil
}

func decodeDepth(b []byte) (*DepthEvent, error) {
	var w struct {
		E  *string      `json:"e"`
		ET *int64       `json:"E"`
		S  *string      `json:"s"`
		U1 *int64       `json:"U"`
		U2 *int64       `json:"u"`
		B  *[][2]string `json:"b"`
		A  *[][2]string `json:"a"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.E == nil || *w.E != "depthUpdate" || w.U1 == nil || w.U2 == nil || w.B == nil || w.A == nil {
		return nil, errors.New("depthUpdate lacks required fields")
	}
	if *w.U1 > *w.U2 {
		return nil, errors.New("first update id U greater than final update id u")
	}
	ev := &DepthEvent{FirstUpdateID: *w.U1, FinalUpdateID: *w.U2, Bids: *w.B, Asks: *w.A}
	var err error
	if ev.EventTime, err = ms(w.ET, "E"); err != nil {
		return nil, err
	}
	if ev.Symbol, err = req(w.S, "s"); err != nil {
		return nil, err
	}
	return ev, nil
}

func decodeKline(b []byte) (*KlineEvent, error) {
	var w struct {
		E  *string `json:"e"`
		ET *int64  `json:"E"`
		S  *string `json:"s"`
		K  *struct {
			T  *int64  `json:"t"`
			CT *int64  `json:"T"`
			S  *string `json:"s"`
			I  *string `json:"i"`
			O  *string `json:"o"`
			C  *string `json:"c"`
			H  *string `json:"h"`
			L  *string `json:"l"`
			V  *string `json:"v"`
			N  *int64  `json:"n"`
			X  *bool   `json:"x"`
			Q  *string `json:"q"`
			// Declared so they cannot match the lowercase keys above.
			F  json.RawMessage `json:"f"`
			LI json.RawMessage `json:"L"`
			VI json.RawMessage `json:"V"`
			QI json.RawMessage `json:"Q"`
			BI json.RawMessage `json:"B"`
		} `json:"k"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.E == nil || *w.E != "kline" || w.K == nil || w.K.N == nil || w.K.X == nil {
		return nil, errors.New("kline lacks required fields")
	}
	k := w.K
	ev := &KlineEvent{Trades: *k.N, Closed: *k.X}
	var err error
	if ev.EventTime, err = ms(w.ET, "E"); err != nil {
		return nil, err
	}
	if ev.OpenTime, err = ms(k.T, "k.t"); err != nil {
		return nil, err
	}
	if ev.CloseTime, err = ms(k.CT, "k.T"); err != nil {
		return nil, err
	}
	if !ev.CloseTime.After(ev.OpenTime) {
		return nil, errors.New("kline close time not after open time")
	}
	for _, f := range []struct {
		src  *string
		dst  *string
		name string
	}{{k.S, &ev.Symbol, "k.s"}, {k.I, &ev.Interval, "k.i"}, {k.O, &ev.Open, "k.o"}, {k.H, &ev.High, "k.h"},
		{k.L, &ev.Low, "k.l"}, {k.C, &ev.Close, "k.c"}, {k.V, &ev.Volume, "k.v"}, {k.Q, &ev.QuoteVolume, "k.q"}} {
		if *f.dst, err = req(f.src, f.name); err != nil {
			return nil, err
		}
	}
	if w.S != nil && *w.S != ev.Symbol {
		return nil, errors.New("kline symbol mismatch")
	}
	return ev, nil
}
