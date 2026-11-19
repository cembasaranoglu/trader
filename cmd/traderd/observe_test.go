package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBinance serves the REST endpoints observe uses and a combined stream
// that is consistent with the depth snapshot.
func fakeBinance(t *testing.T) (restURL, wsURL string) {
	t.Helper()
	info, err := os.ReadFile("../../internal/exchange/binance/testdata/exchange_info.json")
	if err != nil {
		t.Fatal(err)
	}
	var seq atomic.Int64
	seq.Store(100)
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "7")
		switch r.URL.Path {
		case "/api/v3/exchangeInfo":
			w.Write(info)
		case "/api/v3/depth":
			// Like the real service, the snapshot reflects the current sequence.
			fmt.Fprintf(w, `{"lastUpdateId":%d,"bids":[["90000.00","1.00000"]],"asks":[["90000.01","1.00000"]]}`, seq.Load())
		case "/api/v3/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		case "/api/v3/klines":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rest.Close)
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for i := int64(1); ctx.Err() == nil; i++ {
			id := seq.Add(1)
			msgs := []string{
				fmt.Sprintf(`{"stream":"btcusdt@depth@100ms","data":{"e":"depthUpdate","E":1,"s":"BTCUSDT","U":%d,"u":%d,"b":[["89999.99","%d.00000"]],"a":[]}}`, id, id, i%5+1),
				fmt.Sprintf(`{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1,"s":"BTCUSDT","a":%d,"p":"90000.00","q":"0.01","f":1,"l":1,"T":1,"m":false,"M":true}}`, i),
				fmt.Sprintf(`{"stream":"btcusdt@bookTicker","data":{"u":%d,"s":"BTCUSDT","b":"90000.00","B":"1","a":"90000.01","A":"1"}}`, id),
			}
			for _, m := range msgs {
				if c.Write(ctx, websocket.MessageText, []byte(m)) != nil {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(stream.Close)
	return rest.URL, "ws" + strings.TrimPrefix(stream.URL, "http")
}

func TestObserveEndToEnd(t *testing.T) {
	t.Parallel()
	restURL, wsURL := fakeBinance(t)
	out := filepath.Join(t.TempDir(), "observe.jsonl")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"observe", "-config", "../../configs/example.yaml", "-symbols", "btcusdt",
		"-duration", "2s", "-out", out, "-base-url", restURL, "-market-url", restURL, "-ws-url", wsURL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var final observeReport
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &final); err != nil {
			t.Fatalf("bad JSON line: %v", err)
		}
	}
	if final.Type != "final" || final.Interrupted {
		t.Fatalf("last line %+v", final)
	}
	s, ok := final.Symbols["BTCUSDT"]
	if !ok || s.BookState != "live" || s.BookSnapshots < 1 || s.Trades == 0 || s.Tickers == 0 || s.BookUpdateID <= 100 {
		t.Fatalf("symbol report %+v\nstderr: %s", s, stderr.String())
	}
	if final.WS.Dials < 1 || final.WS.Malformed != 0 {
		t.Fatalf("ws report %+v", final.WS)
	}
	if !strings.Contains(stdout.String(), "BTCUSDT book=live") {
		t.Fatalf("summary: %s", stdout.String())
	}
}

func TestObserveUnknownSymbol(t *testing.T) {
	t.Parallel()
	restURL, wsURL := fakeBinance(t)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"observe", "-config", "../../configs/example.yaml", "-symbols", "NOPEUSDT",
		"-duration", "1s", "-base-url", restURL, "-market-url", restURL, "-ws-url", wsURL}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "unknown symbols: NOPEUSDT") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
}

func TestObserveInterrupted(t *testing.T) {
	t.Parallel()
	restURL, wsURL := fakeBinance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"observe", "-config", "../../configs/example.yaml", "-symbols", "BTCUSDT",
		"-duration", "1h", "-base-url", restURL, "-market-url", restURL, "-ws-url", wsURL}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"interrupted":true`) {
		t.Fatalf("exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
}
