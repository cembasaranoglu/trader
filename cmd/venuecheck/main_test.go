package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runOK(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), append(args, "-samples", "1", "-out", out), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(readFixture(t, out), &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	return rep
}

func TestRunBinance(t *testing.T) {
	t.Parallel()
	const fx = "../../internal/exchange/binance/testdata/"
	info, tickers, depth, klines := readFixture(t, fx+"exchange_info.json"), readFixture(t, fx+"ticker_mini.json"),
		readFixture(t, fx+"depth.json"), readFixture(t, fx+"klines.json")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/time", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"serverTime":1700000000000}`) })
	mux.HandleFunc("/api/v3/exchangeInfo", func(w http.ResponseWriter, r *http.Request) { w.Write(info) })
	mux.HandleFunc("/api/v3/ticker/24hr", func(w http.ResponseWriter, r *http.Request) { w.Write(tickers) })
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) { w.Write(depth) })
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) { w.Write(klines) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rep := runOK(t, "-base-url", srv.URL, "-market-url", srv.URL, "-symbols", "BTCUSDT")
	if rep["venue"] != "binance" || rep["account_skipped"] == nil {
		t.Fatalf("report %v", rep)
	}
	top := rep["top_by_quote_volume_24h"].([]any)
	if len(top) != 1 || top[0].(map[string]any)["symbol"] != "BTCUSDT" {
		t.Fatalf("top %v", top)
	}
}

func TestRunBinanceTR(t *testing.T) {
	t.Parallel()
	symbols := readFixture(t, "../../internal/exchange/binancetr/testdata/symbols.json")
	mux := http.NewServeMux()
	mux.HandleFunc("/open/v1/common/time", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"msg":"success","timestamp":1700000000000}`)
	})
	mux.HandleFunc("/open/v1/common/symbols", func(w http.ResponseWriter, r *http.Request) { w.Write(symbols) })
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"lastUpdateId":1,"bids":[["100","1"]],"asks":[["101","1"]]}`)
	})
	mux.HandleFunc("/api/v1/klines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[[1,"1","1","1","1","1",2,"1",1,"1","1","0"]]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rep := runOK(t, "-venue", "binance_tr", "-base-url", srv.URL, "-market-url", srv.URL, "-symbols", "BTC_TRY")
	sym := rep["symbols"].(map[string]any)
	if rep["venue"] != "binance_tr" || sym["quote"] != "TRY" || len(rep["watched"].([]any)) != 1 {
		t.Fatalf("report %v", rep)
	}
}

func TestRunUsageAndCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string, perm os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
		return p
	}
	open := write("open.json", `{"api_key":"k","secret_key":"s"}`, 0o644)
	pem := write("k.pem", "not a key", 0o600)
	ed := write("ed.json", `{"api_key":"k","ed25519_private_key_file":"`+pem+`"}`, 0o600)
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"extra arg", []string{"x"}, 2, "usage"},
		{"unknown venue", []string{"-venue", "kraken"}, 2, "usage"},
		{"bad samples", []string{"-samples", "0"}, 2, "usage"},
		{"bad top", []string{"-top", "101"}, 2, "usage"},
		{"unknown flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"world-readable credentials", []string{"-credentials", open}, 1, "chmod 600"},
		{"invalid ed25519 pem", []string{"-credentials", ed}, 1, "PEM"},
		{"ed25519 on binance_tr", []string{"-venue", "binance_tr", "-credentials", ed}, 1, "only HMAC"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("exit %d, want %d (%s)", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr %q missing %q", stderr.String(), tc.wantErr)
			}
		})
	}
}
