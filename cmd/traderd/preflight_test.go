package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/store"
)

// tempConfig writes the example config with store paths inside dir.
func tempConfig(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile("../../configs/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "path: data/trader.db", "path: "+filepath.Join(dir, "db", "trader.db"), 1)
	s = strings.Replace(s, "backup_dir: data/backups", "backup_dir: "+filepath.Join(dir, "backups"), 1)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func credFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"k","secret_key":"s3cr3t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, 0o600)
	return p
}

func fakeSigned(t *testing.T, restrictions string) string {
	t.Helper()
	info, err := os.ReadFile("../../internal/exchange/binance/testdata/exchange_info.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		case "/api/v3/exchangeInfo":
			w.Write(info)
		case "/sapi/v1/account/apiRestrictions":
			fmt.Fprint(w, restrictions)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const goodRestrictions = `{"ipRestrict":true,"enableReading":true,"enableSpotAndMarginTrading":true,"enableWithdrawals":false}`

func TestPreflightCommand(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("credential permission checks are Unix-only")
	}
	dir := t.TempDir()
	cfg, creds := tempConfig(t, dir), credFile(t, dir)
	for name, tc := range map[string]struct {
		restrictions string
		want         int
		out          string
	}{
		"pass":        {goodRestrictions, 0, "preflight ok"},
		"withdrawals": {strings.Replace(goodRestrictions, `"enableWithdrawals":false`, `"enableWithdrawals":true`, 1), 1, "FAIL key_permissions"},
	} {
		t.Run(name, func(t *testing.T) {
			url := fakeSigned(t, tc.restrictions)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"preflight", "-config", cfg, "-credentials", creds, "-symbols", "BTCUSDT",
				"-base-url", url, "-market-url", url}, &stdout, &stderr)
			if code != tc.want || !strings.Contains(stdout.String(), tc.out) {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "s3cr3t") {
				t.Fatal("secret printed")
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"preflight", "-config", cfg, "-symbols", "BTCUSDT"}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing credentials must be a usage error, got %d", code)
	}
	if code := run(context.Background(), []string{"preflight", "-config", cfg, "-credentials", creds, "-symbols", "BTCUSDT", "-environment", "staging"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown environment must be a usage error, got %d", code)
	}
}

func TestStoreCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := tempConfig(t, dir)
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"store", "verify", "-config", cfg}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "no database") {
		t.Fatalf("verify without a database: exit %d %s", code, stderr.String())
	}
	s, err := store.Open(context.Background(), filepath.Join(dir, "db", "trader.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Append(context.Background(), "test", map[string]int{"v": 1, "i": i}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	stdout.Reset()
	if code := run(context.Background(), []string{"store", "verify", "-config", cfg}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "store ok") {
		t.Fatalf("verify: exit %d %s %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"store", "backup", "-config", cfg}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "backup ok") {
		t.Fatalf("backup: exit %d %s %s", code, stdout.String(), stderr.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "backups", "trader-*.db"))
	if len(matches) != 1 {
		t.Fatalf("backups %v", matches)
	}
	if err := store.VerifyFile(context.Background(), matches[0]); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), []string{"store", "compact", "-config", cfg}, &stdout, &stderr); code != 2 {
		t.Fatal("unknown store subcommand must be a usage error")
	}
}
