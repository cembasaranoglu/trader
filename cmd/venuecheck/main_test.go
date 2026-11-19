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

func TestRunAgainstFakeVenue(t *testing.T) {
	t.Parallel()
	symbols, err := os.ReadFile("../../internal/exchange/binancetr/testdata/symbols.json")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/open/v1/common/time", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"msg":"success","timestamp":1700000000000}`)
	})
	mux.HandleFunc("/open/v1/common/symbols", func(w http.ResponseWriter, r *http.Request) { w.Write(symbols) })
	mux.HandleFunc("/api/v3/depth", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"lastUpdateId":1,"bids":[["100","1"]],"asks":[["101","1"]]}`)
	})
	mux.HandleFunc("/api/v1/klines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":[[1,"1","1","1","1","1",2,"1",1,"1","1","0"]]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-base-url", srv.URL, "-main-market-url", srv.URL, "-next-market-url", srv.URL,
		"-symbols", "BTC_TRY", "-samples", "1", "-out", out,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if rep["account_skipped"] == nil || len(rep["watched"].([]any)) != 1 {
		t.Fatalf("unexpected report: %s", b)
	}
}

func TestRunUsageAndCredentials(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(open, []byte(`{"api_key":"k","secret_key":"s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"extra arg", []string{"x"}, 2, "usage"},
		{"bad samples", []string{"-samples", "0"}, 2, "usage"},
		{"unknown flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"world-readable credentials", []string{"-credentials", open}, 1, "chmod 600"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tc.args, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("exit %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("stderr %q missing %q", stderr.String(), tc.wantErr)
			}
		})
	}
}
