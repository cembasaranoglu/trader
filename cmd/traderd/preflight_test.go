package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/pgtest"
	"github.com/cembasaranoglu/trader/internal/store"
)

// tempConfig writes the example config with the store pointed at a fresh,
// migrated test database (skips without TRADER_TEST_PG).
func tempConfig(t *testing.T, dir string) string {
	t.Helper()
	d := pgtest.New(t)
	return tempConfigFor(t, dir, d, true)
}

func tempConfigFor(t *testing.T, dir string, d *pgtest.DB, migrate bool) string {
	t.Helper()
	if migrate {
		if _, err := store.Migrate(context.Background(), d.Owner, d.AppRole); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, 0o600)
		return p
	}
	b, err := os.ReadFile("../../configs/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for old, repl := range map[string]string{
		"dsn_file: secrets/db-app.dsn":           "dsn_file: " + secret("app.dsn", d.App),
		"migrate_dsn_file: secrets/db-owner.dsn": "migrate_dsn_file: " + secret("owner.dsn", d.Owner),
		"app_role: trader_app":                   "app_role: " + d.AppRole,
		"anchor_path: data/trader.anchor":        "anchor_path: " + filepath.Join(dir, "trader.anchor"),
		"export_dir: data/exports":               "export_dir: " + filepath.Join(dir, "exports"),
	} {
		if !strings.Contains(s, old) {
			t.Fatalf("example config lacks %q", old)
		}
		s = strings.Replace(s, old, repl, 1)
	}
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
	// Demo skips the permission check, so it must not be pointed at the live host.
	if code := run(context.Background(), []string{"preflight", "-config", cfg, "-credentials", creds, "-symbols", "BTCUSDT",
		"-environment", "demo", "-base-url", "https://api.binance.com"}, &stdout, &stderr); code != 2 {
		t.Fatalf("demo with a live host must be refused, got %d", code)
	}
}

func TestStoreCommands(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("secret file permission checks are Unix-only")
	}
	dir := t.TempDir()
	d := pgtest.New(t)
	cfg := tempConfigFor(t, dir, d, false)
	ctx := context.Background()
	runOK := func(want string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(ctx, args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), want) {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	runFail := func(code int, want string, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(ctx, args, &stdout, &stderr); got != code || !strings.Contains(stderr.String(), want) {
			t.Fatalf("%v: exit %d (want %d) stderr %q", args, got, code, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), ":pw@") {
			t.Fatal("a connection string password was printed")
		}
	}
	runFail(1, "older than this binary", "store", "verify", "-config", cfg)
	runOK("migrate ok: applied [1]", "store", "migrate", "-config", cfg)
	runOK("migrate ok: applied []", "store", "migrate", "-config", cfg)
	runOK("store ok", "store", "verify", "-config", cfg)
	// dsn_file must name the application role: the owner is refused.
	ownerCfg := filepath.Join(dir, "config-owner.yaml")
	ownerCopy := filepath.Join(dir, "owner-as-app.dsn")
	if err := os.WriteFile(ownerCopy, []byte(d.Owner+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(cfg); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(ownerCfg, []byte(strings.Replace(string(b), filepath.Join(dir, "app.dsn"), ownerCopy, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	runFail(1, "more privileges", "store", "verify", "-config", ownerCfg)

	s, err := store.Open(ctx, d.App, store.Options{AnchorPath: filepath.Join(dir, "trader.anchor")})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := s.Append(ctx, "test", map[string]int{"v": 1, "i": i}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	runOK("store ok", "store", "verify", "-config", cfg)
	runOK("export ok", "store", "export", "-config", cfg)
	matches, _ := filepath.Glob(filepath.Join(dir, "exports", "trader-events-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("exports %v", matches)
	}
	if fi, err := os.Stat(matches[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("export mode %v %v", fi, err)
	}
	runOK("events=3", "store", "verify", "-export", matches[0])
	f, _ := os.OpenFile(matches[0], os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("x"))
	f.Close()
	runFail(1, "does not match its .sha256", "store", "verify", "-export", matches[0])

	// The anchor is gone (rebuilt host): verify refuses until anchor-init.
	os.Remove(filepath.Join(dir, "trader.anchor"))
	runFail(1, "anchor file missing", "store", "verify", "-config", cfg)
	runOK("anchor ok", "store", "anchor-init", "-config", cfg)
	runFail(1, "already exists", "store", "anchor-init", "-config", cfg)
	runOK("store ok", "store", "verify", "-config", cfg)

	for _, args := range [][]string{
		{"store", "compact", "-config", cfg},
		{"store", "export", "-export", "x"},
		{"store", "verify", "-config", cfg, "-export", "x"},
		{"store", "verify"},
		{"store"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(ctx, args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want usage error", args, code)
		}
	}
}

func TestResolveDSN(t *testing.T) {
	// Not parallel: sets environment variables.
	t.Setenv(envStoreDSN, "postgres://u:pw@db.example.com/db?sslmode=verify-full")
	if _, err := resolveDSN(envStoreDSN, "", io.Discard); err == nil || strings.Contains(err.Error(), "pw@") {
		t.Fatalf("remote host from env: %v", err)
	}
	t.Setenv(envStoreDSN, "postgres://u:pw@postgres:5432/db?sslmode=disable")
	if v, err := resolveDSN(envStoreDSN, "", io.Discard); err != nil || !strings.HasPrefix(v, "postgres://u:pw@postgres") {
		t.Fatalf("compose host from env: %q %v", v, err)
	}
	ca := pgtest.CAFile(t)
	for _, d := range []string{
		"postgres://u:pw@db.example.com/db?sslmode=verify-full&sslrootcert=" + ca,
		"postgres://u:pw@localhost:1,db.example.com:5432/db?sslmode=verify-full&sslrootcert=" + ca,
	} {
		t.Setenv(envStoreDSN, d)
		if _, err := resolveDSN(envStoreDSN, "", io.Discard); err == nil || !strings.Contains(err.Error(), "local databases") {
			t.Fatalf("remote host from env: %v", err)
		}
	}
	t.Setenv(envStoreDSN, "")
	if _, err := resolveDSN(envStoreDSN, "", io.Discard); err == nil {
		t.Fatal("no source accepted")
	}
	if _, err := resolveDSN(envStoreDSN, filepath.Join(t.TempDir(), "missing"), io.Discard); err == nil {
		t.Fatal("missing file accepted")
	}
}
