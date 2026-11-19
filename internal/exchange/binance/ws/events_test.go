package ws

import (
	"errors"
	"testing"
	"time"
)

func TestDecodeEvents(t *testing.T) {
	t.Parallel()
	kline := `{"stream":"btcusdt@kline_1m","data":{"e":"kline","E":1672515782136,"s":"BTCUSDT","k":{"t":1672515780000,"T":1672515839999,"s":"BTCUSDT","i":"1m","f":100,"L":200,"o":"0.0010","c":"0.0020","h":"0.0025","l":"0.0015","v":"1000","n":100,"x":true,"q":"1.0000","V":"500","Q":"0.500","B":"123456"}}}`
	_, ev, err := Decode([]byte(kline))
	if err != nil {
		t.Fatal(err)
	}
	k := ev.(*KlineEvent)
	if !k.Closed || k.Interval != "1m" || k.Trades != 100 || k.OpenTime != time.UnixMilli(1672515780000).UTC() || k.High != "0.0025" {
		t.Fatalf("%+v", k)
	}

	depth := `{"stream":"bnbbtc@depth@100ms","data":{"e":"depthUpdate","E":1672515782136,"s":"BNBBTC","U":157,"u":160,"b":[["0.0024","10"]],"a":[["0.0026","100"]]}}`
	_, ev, err = Decode([]byte(depth))
	if err != nil {
		t.Fatal(err)
	}
	d := ev.(*DepthEvent)
	if d.FirstUpdateID != 157 || d.FinalUpdateID != 160 || d.Bids[0] != [2]string{"0.0024", "10"} || len(d.Asks) != 1 {
		t.Fatalf("%+v", d)
	}

	bt := `{"stream":"bnbusdt@bookTicker","data":{"u":400900217,"s":"BNBUSDT","b":"25.35190000","B":"31.21000000","a":"25.36520000","A":"40.66000000"}}`
	_, ev, err = Decode([]byte(bt))
	if err != nil || ev.(*BookTickerEvent).UpdateID != 400900217 {
		t.Fatalf("%v %+v", err, ev)
	}

	_, ev, err = Decode([]byte(shutdownMsg))
	if err != nil || ev.(*ServerShutdownEvent).ShutdownTime != time.UnixMilli(1700000600000).UTC() {
		t.Fatalf("%v %+v", err, ev)
	}
	// The raw (non-combined) form is documented too.
	if stream, ev, err := Decode([]byte(`{"e":"serverShutdown","E":1770123456789,"T":1770124000000}`)); err != nil || stream != "!serverShutdown" || ev.(*ServerShutdownEvent).EventTime.IsZero() {
		t.Fatalf("raw serverShutdown: %v %q", err, stream)
	}
	// T is optional (added in a later API version).
	if _, _, err := Decode([]byte(`{"stream":"!serverShutdown","data":{"e":"serverShutdown","E":1}}`)); err != nil {
		t.Fatal(err)
	}
}

// TestDecodeCaseCollisions guards against encoding/json's case-insensitive
// key matching: "M" (ignore) must not overwrite "m" (buyer is maker), and the
// kline's "L", "V", "Q" must not land in "l", "v", "q".
func TestDecodeCaseCollisions(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`{"stream":"a@aggTrade","data":{"e":"aggTrade","E":1,"s":"A","a":1,"p":"1","q":"1","f":1,"l":1,"T":1,"m":false,"M":true}}`,
		`{"stream":"a@aggTrade","data":{"e":"aggTrade","E":1,"s":"A","a":1,"p":"1","q":"1","f":1,"l":1,"T":1,"M":true,"m":false}}`,
	} {
		_, ev, err := Decode([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if ev.(*AggTradeEvent).BuyerMaker {
			t.Fatalf("BuyerMaker overwritten by the ignored M field: %s", in)
		}
	}
	k := `{"stream":"a@kline_1m","data":{"e":"kline","E":1,"s":"A","k":{"V":"999","Q":"888","L":777,"t":1,"T":2,"s":"A","i":"1m","f":1,"o":"1","c":"2","h":"3","l":"0.5","v":"10","n":1,"x":false,"q":"20","B":"0"}}}`
	_, ev, err := Decode([]byte(k))
	if err != nil {
		t.Fatal(err)
	}
	if kl := ev.(*KlineEvent); kl.Low != "0.5" || kl.Volume != "10" || kl.QuoteVolume != "20" {
		t.Fatalf("lowercase kline fields overwritten: %+v", kl)
	}
}

func TestDecodeRejects(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":                  ``,
		"array":                  `[]`,
		"no stream":              `{"data":{}}`,
		"no data":                `{"stream":"a@aggTrade"}`,
		"null data":              `{"stream":"a@aggTrade","data":null}`,
		"unsupported":            `{"stream":"a@trade","data":{}}`,
		"agg wrong type":         `{"stream":"a@aggTrade","data":{"e":"trade","E":1,"s":"A","a":1,"p":"1","q":"1","f":1,"l":1,"T":1,"m":true}}`,
		"agg missing price":      `{"stream":"a@aggTrade","data":{"e":"aggTrade","E":1,"s":"A","a":1,"q":"1","f":1,"l":1,"T":1,"m":true}}`,
		"agg missing time":       `{"stream":"a@aggTrade","data":{"e":"aggTrade","E":1,"s":"A","a":1,"p":"1","q":"1","f":1,"l":1,"m":true}}`,
		"depth U > u":            `{"stream":"a@depth","data":{"e":"depthUpdate","E":1,"s":"A","U":5,"u":4,"b":[],"a":[]}}`,
		"depth no bids":          `{"stream":"a@depth","data":{"e":"depthUpdate","E":1,"s":"A","U":1,"u":2,"a":[]}}`,
		"depth bad level":        `{"stream":"a@depth","data":{"e":"depthUpdate","E":1,"s":"A","U":1,"u":2,"b":[[1,2]],"a":[]}}`,
		"ticker no u":            `{"stream":"a@bookTicker","data":{"s":"A","b":"1","B":"1","a":"1","A":"1"}}`,
		"ticker empty ask":       `{"stream":"a@bookTicker","data":{"u":1,"s":"A","b":"1","B":"1","a":"","A":"1"}}`,
		"kline no x":             `{"stream":"a@kline_1m","data":{"e":"kline","E":1,"s":"A","k":{"t":1,"T":2,"s":"A","i":"1m","o":"1","c":"1","h":"1","l":"1","v":"1","n":1,"q":"1"}}}`,
		"kline close<=open":      `{"stream":"a@kline_1m","data":{"e":"kline","E":1,"s":"A","k":{"t":2,"T":2,"s":"A","i":"1m","o":"1","c":"1","h":"1","l":"1","v":"1","n":1,"x":true,"q":"1"}}}`,
		"kline symbol differ":    `{"stream":"a@kline_1m","data":{"e":"kline","E":1,"s":"B","k":{"t":1,"T":2,"s":"A","i":"1m","o":"1","c":"1","h":"1","l":"1","v":"1","n":1,"x":true,"q":"1"}}}`,
		"shutdown wrong e":       `{"stream":"!serverShutdown","data":{"e":"x","E":1}}`,
		"stream symbol differs":  `{"stream":"ethusdt@aggTrade","data":{"e":"aggTrade","E":1,"s":"BTCUSDT","a":1,"p":"1","q":"1","f":1,"l":1,"T":1,"m":true}}`,
		"depth symbol differs":   `{"stream":"ethusdt@depth@100ms","data":{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":1,"u":2,"b":[],"a":[]}}`,
		"kline interval differs": `{"stream":"a@kline_1h","data":{"e":"kline","E":1,"s":"A","k":{"t":1,"T":2,"s":"A","i":"1m","o":"1","c":"1","h":"1","l":"1","v":"1","n":1,"x":true,"q":"1"}}}`,
	} {
		if _, _, err := Decode([]byte(in)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(shutdownMsg))
	f.Add([]byte(`{"stream":"a@depth","data":{"e":"depthUpdate","E":1,"s":"A","U":1,"u":2,"b":[["1","2"]],"a":[]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		stream, ev, err := Decode(b)
		if err == nil && (stream == "" || ev == nil) {
			t.Fatal("success without stream or event")
		}
	})
}
