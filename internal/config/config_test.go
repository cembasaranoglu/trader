package config_test

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cembasaranoglu/trader/internal/config"
)

const examplePath = "../../configs/example.yaml"

func readExample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return b
}

// exampleMap returns the example config as a generic map so tests can mutate it.
func exampleMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(readExample(t), &m); err != nil {
		t.Fatalf("unmarshal example: %v", err)
	}
	return m
}

// walk returns the parent map of a dotted path and the final key.
func walk(t *testing.T, m map[string]any, path string) (map[string]any, string) {
	t.Helper()
	parts := strings.Split(path, ".")
	cur := m
	for _, p := range parts[:len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("path %q: %q is not a map", path, p)
		}
		cur = next
	}
	return cur, parts[len(parts)-1]
}

func set(t *testing.T, m map[string]any, path string, v any) {
	t.Helper()
	parent, key := walk(t, m, path)
	parent[key] = v
}

func del(t *testing.T, m map[string]any, path string) {
	t.Helper()
	parent, key := walk(t, m, path)
	if _, ok := parent[key]; !ok {
		t.Fatalf("path %q not present in example", path)
	}
	delete(parent, key)
}

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestLoadFileExample(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFile(examplePath)
	if err != nil {
		t.Fatalf("example config must load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"mode", cfg.Mode, config.ModePaper},
		{"venue.primary", cfg.Venue.Primary, "binance"},
		{"quote", cfg.QuoteCurrency, "USDT"},
		{"max_drawdown", cfg.Risk.MaxDrawdownPct, 30.0},
		{"on_max_dd", cfg.Risk.OnMaxDD, config.OnMaxDDCloseAll},
		{"data_age", cfg.Risk.MaxDataAge, 5 * time.Second},
		{"entry_window", cfg.Risk.MaxEntriesPerWindow.Window, 60 * time.Minute},
		{"entry_count", cfg.Risk.MaxEntriesPerWindow.Count, 3},
		{"stp", cfg.Execution.STPMode, config.STPExpireTaker},
		{"heartbeat", cfg.Ops.HeartbeatTimeout, 30 * time.Second},
		{"clock_skew", cfg.Ops.MaxClockSkew, 500 * time.Millisecond},
		{"routine_model", cfg.LLM.Models.Routine, "claude-haiku-5-5"},
		{"thesis_escalations", cfg.Agents.Thesis.MaxEscalationsPerDay, 2},
		{"reflection", cfg.Agents.Reflection.Enabled, true},
		{"g2_days", cfg.Gates.G2MinDays, 180},
		{"depth_limit", cfg.MarketData.DepthLimit, 1000},
		{"intervals", strings.Join(cfg.MarketData.KlineIntervals, ","), "1m,1h,4h,1d"},
		{"reconnect_backoff", cfg.MarketData.ReconnectMaxBackoff, 60 * time.Second},
		{"store_path", cfg.Store.Path, mustAbs(t, "../../configs/data/trader.db")},
		{"backup_dir", cfg.Store.BackupDir, mustAbs(t, "../../configs/data/backups")},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mutate   func(t *testing.T, m map[string]any)
		wantErr  error
		wantPath string // checked when wantErr is ErrMissing or ErrInvalid
	}{
		{"missing top-level section", func(t *testing.T, m map[string]any) { del(t, m, "risk") }, config.ErrMissing, "risk"},
		{"missing nested field", func(t *testing.T, m map[string]any) { del(t, m, "risk.max_daily_loss_pct") }, config.ErrMissing, "risk.max_daily_loss_pct"},
		{"missing bool is not treated as false", func(t *testing.T, m map[string]any) { del(t, m, "agents.critic.enabled") }, config.ErrMissing, "agents.critic.enabled"},
		{"missing window", func(t *testing.T, m map[string]any) { del(t, m, "risk.max_entries_per_window.window") }, config.ErrMissing, "risk.max_entries_per_window.window"},
		{"unknown mode", func(t *testing.T, m map[string]any) { set(t, m, "mode", "live") }, config.ErrInvalid, "mode"},
		{"live_small not yet supported", func(t *testing.T, m map[string]any) { set(t, m, "mode", "live_small") }, config.ErrInvalid, "mode"},
		{"live_full not yet supported", func(t *testing.T, m map[string]any) { set(t, m, "mode", "live_full") }, config.ErrInvalid, "mode"},
		{"unsupported venue", func(t *testing.T, m map[string]any) { set(t, m, "venue.primary", "kraken") }, config.ErrInvalid, "venue.primary"},
		{"secondary venue before W9", func(t *testing.T, m map[string]any) { set(t, m, "venue.secondary_enabled", true) }, config.ErrInvalid, "venue.secondary_enabled"},
		{"unsupported quote", func(t *testing.T, m map[string]any) { set(t, m, "quote_currency", "EUR") }, config.ErrInvalid, "quote_currency"},
		{"TRY quote not supported", func(t *testing.T, m map[string]any) { set(t, m, "quote_currency", "TRY") }, config.ErrInvalid, "quote_currency"},
		{"binance_tr primary not supported", func(t *testing.T, m map[string]any) { set(t, m, "venue.primary", "binance_tr") }, config.ErrInvalid, "venue.primary"},
		{"drawdown below D-11 floor", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_drawdown_pct", 29.9) }, config.ErrInvalid, "risk.max_drawdown_pct"},
		{"drawdown above 100", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_drawdown_pct", 101) }, config.ErrInvalid, "risk.max_drawdown_pct"},
		{"zero daily loss", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_daily_loss_pct", 0) }, config.ErrInvalid, "risk.max_daily_loss_pct"},
		{"negative order notional", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_order_notional_usd", -1) }, config.ErrInvalid, "risk.max_order_notional_usd"},
		{"daily loss above weekly", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_daily_loss_pct", 8) }, config.ErrInvalid, "risk.max_daily_loss_pct"},
		{"weekly loss above drawdown", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_weekly_loss_pct", 31) }, config.ErrInvalid, "risk.max_weekly_loss_pct"},
		{"per-trade risk above daily loss", func(t *testing.T, m map[string]any) { set(t, m, "risk.per_trade_risk_pct", 4) }, config.ErrInvalid, "risk.per_trade_risk_pct"},
		{"position above gross exposure", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_position_pct", 90) }, config.ErrInvalid, "risk.max_position_pct"},
		{"order notional above live capital", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_order_notional_usd", 201) }, config.ErrInvalid, "risk.max_order_notional_usd"},
		{"live capital above planned", func(t *testing.T, m map[string]any) { set(t, m, "capital.live_small_max_usd", 300) }, config.ErrInvalid, "capital.live_small_max_usd"},
		{"cash reserve 100", func(t *testing.T, m map[string]any) { set(t, m, "risk.cash_reserve_pct", 100) }, config.ErrInvalid, "risk.cash_reserve_pct"},
		{"edge ratio below 1", func(t *testing.T, m map[string]any) { set(t, m, "risk.min_edge_to_cost_ratio", 0.5) }, config.ErrInvalid, "risk.min_edge_to_cost_ratio"},
		{"negative reconcile tolerance", func(t *testing.T, m map[string]any) { set(t, m, "risk.reconcile_tolerance_steps", -1) }, config.ErrInvalid, "risk.reconcile_tolerance_steps"},
		{"zero entry window", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_entries_per_window.window", "0s") }, config.ErrInvalid, "risk.max_entries_per_window.window"},
		{"stp NONE rejected", func(t *testing.T, m map[string]any) { set(t, m, "execution.stp_mode", "NONE") }, config.ErrInvalid, "execution.stp_mode"},
		{"zero heartbeat", func(t *testing.T, m map[string]any) { set(t, m, "ops.heartbeat_timeout_s", 0) }, config.ErrInvalid, "ops.heartbeat_timeout_s"},
		{"empty model id", func(t *testing.T, m map[string]any) { set(t, m, "llm.models.routine", "") }, config.ErrInvalid, "llm.models.routine"},
		{"escalations above calls", func(t *testing.T, m map[string]any) { set(t, m, "agents.thesis.max_escalations_per_day", 21) }, config.ErrInvalid, "agents.thesis.max_escalations_per_day"},
		{"negative call cap", func(t *testing.T, m map[string]any) { set(t, m, "agents.regime.max_calls_per_day", -1) }, config.ErrInvalid, "agents.regime.max_calls_per_day"},
		{"zero supporting trades", func(t *testing.T, m map[string]any) { set(t, m, "learning.min_supporting_trades", 0) }, config.ErrInvalid, "learning.min_supporting_trades"},
		{"edge multiple below 1", func(t *testing.T, m map[string]any) { set(t, m, "gates.edge_cost_multiple", 0.9) }, config.ErrInvalid, "gates.edge_cost_multiple"},
		{"unknown key", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_leverage", 3) }, config.ErrDecode, ""},
		{"unknown top-level key", func(t *testing.T, m map[string]any) { m["api_key"] = "x" }, config.ErrDecode, ""},
		{"window without unit", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_entries_per_window.window", 60) }, config.ErrDecode, ""},
		{"wrong scalar type", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_orders_per_min", "ten") }, config.ErrDecode, ""},
		{"infinite drawdown", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_drawdown_pct", math.Inf(1)) }, config.ErrInvalid, "risk.max_drawdown_pct"},
		{"NaN order notional", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_order_notional_usd", math.NaN()) }, config.ErrInvalid, "risk.max_order_notional_usd"},
		{"infinite planned capital", func(t *testing.T, m map[string]any) { set(t, m, "capital.planned_usd", math.Inf(1)) }, config.ErrInvalid, "capital.planned_usd"},
		{"negative infinite reserve", func(t *testing.T, m map[string]any) { set(t, m, "risk.cash_reserve_pct", math.Inf(-1)) }, config.ErrInvalid, "risk.cash_reserve_pct"},
		{"collar above 100%", func(t *testing.T, m map[string]any) { set(t, m, "risk.price_collar_bps", 10001) }, config.ErrInvalid, "risk.price_collar_bps"},
		{"data age above 60s", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_data_age_ms", 60001) }, config.ErrInvalid, "risk.max_data_age_ms"},
		{"data age overflow", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_data_age_ms", int64(1)<<62) }, config.ErrDecode, ""},
		{"clock skew above recvWindow max", func(t *testing.T, m map[string]any) { set(t, m, "ops.max_clock_skew_ms", 60001) }, config.ErrInvalid, "ops.max_clock_skew_ms"},
		{"heartbeat above 1h", func(t *testing.T, m map[string]any) { set(t, m, "ops.heartbeat_timeout_s", 3601) }, config.ErrInvalid, "ops.heartbeat_timeout_s"},
		{"window below 1m", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_entries_per_window.window", "59s") }, config.ErrInvalid, "risk.max_entries_per_window.window"},
		{"window above 24h", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_entries_per_window.window", "25h") }, config.ErrInvalid, "risk.max_entries_per_window.window"},
		{"float for integer", func(t *testing.T, m map[string]any) { set(t, m, "risk.max_orders_per_min", 10.9) }, config.ErrDecode, ""},
		{"string for bool", func(t *testing.T, m map[string]any) { set(t, m, "agents.critic.enabled", "true") }, config.ErrDecode, ""},
		{"whitespace model id", func(t *testing.T, m map[string]any) { set(t, m, "llm.models.synthesis", "   ") }, config.ErrInvalid, "llm.models.synthesis"},
		{"padded model id", func(t *testing.T, m map[string]any) { set(t, m, "llm.models.synthesis", " claude-sonnet-5-5") }, config.ErrInvalid, "llm.models.synthesis"},
		{"missing marketdata", func(t *testing.T, m map[string]any) { del(t, m, "marketdata") }, config.ErrMissing, "marketdata"},
		{"depth limit 0", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.depth_limit", 0) }, config.ErrInvalid, "marketdata.depth_limit"},
		{"depth limit 5001", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.depth_limit", 5001) }, config.ErrInvalid, "marketdata.depth_limit"},
		{"no intervals", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.kline_intervals", []any{}) }, config.ErrInvalid, "marketdata.kline_intervals"},
		{"unsupported interval", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.kline_intervals", []any{"2m"}) }, config.ErrInvalid, "marketdata.kline_intervals"},
		{"duplicate interval", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.kline_intervals", []any{"1m", "1m"}) }, config.ErrInvalid, "marketdata.kline_intervals"},
		{"intervals not a list", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.kline_intervals", "1m") }, config.ErrDecode, ""},
		{"backoff 0", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.reconnect_max_backoff_s", 0) }, config.ErrInvalid, "marketdata.reconnect_max_backoff_s"},
		{"backoff 301", func(t *testing.T, m map[string]any) { set(t, m, "marketdata.reconnect_max_backoff_s", 301) }, config.ErrInvalid, "marketdata.reconnect_max_backoff_s"},
		{"missing store", func(t *testing.T, m map[string]any) { del(t, m, "store") }, config.ErrMissing, "store"},
		{"blank store path", func(t *testing.T, m map[string]any) { set(t, m, "store.path", " ") }, config.ErrInvalid, "store.path"},
		{"backup dir equals db dir", func(t *testing.T, m map[string]any) { set(t, m, "store.backup_dir", "data/") }, config.ErrInvalid, "store.backup_dir"},
		{"backup dir equals db file", func(t *testing.T, m map[string]any) { set(t, m, "store.backup_dir", "data/trader.db") }, config.ErrInvalid, "store.backup_dir"},
		{"backup dir contains db", func(t *testing.T, m map[string]any) { set(t, m, "store.backup_dir", ".") }, config.ErrInvalid, "store.backup_dir"},
		{"gross plus reserve above 100", func(t *testing.T, m map[string]any) { set(t, m, "risk.cash_reserve_pct", 25) }, config.ErrInvalid, "risk.max_gross_exposure_pct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := exampleMap(t)
			tc.mutate(t, m)
			_, err := config.Load(bytes.NewReader(marshal(t, m)))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v does not wrap %v", err, tc.wantErr)
			}
			if tc.wantPath == "" {
				return
			}
			if !hasFieldError(err, tc.wantPath, tc.wantErr) {
				t.Fatalf("no FieldError for path %q in: %v", tc.wantPath, err)
			}
		})
	}
}

// hasFieldError reports whether a joined error contains a FieldError for path.
func hasFieldError(err error, path string, sentinel error) bool {
	multi, ok := err.(interface{ Unwrap() []error })
	if !ok {
		var fe *config.FieldError
		return errors.As(err, &fe) && fe.Path == path && errors.Is(fe, sentinel)
	}
	for _, e := range multi.Unwrap() {
		var fe *config.FieldError
		if errors.As(e, &fe) && fe.Path == path && errors.Is(fe, sentinel) {
			return true
		}
	}
	return false
}

func TestLoadReportsEveryProblem(t *testing.T) {
	t.Parallel()
	m := exampleMap(t)
	del(t, m, "mode")
	set(t, m, "risk.max_drawdown_pct", 10)
	del(t, m, "gates.paper_min_days")
	_, err := config.Load(bytes.NewReader(marshal(t, m)))
	for _, want := range []struct {
		path string
		err  error
	}{
		{"mode", config.ErrMissing},
		{"risk.max_drawdown_pct", config.ErrInvalid},
		{"gates.paper_min_days", config.ErrMissing},
	} {
		if !hasFieldError(err, want.path, want.err) {
			t.Errorf("missing %v for %q in: %v", want.err, want.path, err)
		}
	}
}

func TestLoadDocumentShape(t *testing.T) {
	t.Parallel()
	example := readExample(t)
	tests := []struct {
		name  string
		input []byte
	}{
		{"empty", nil},
		{"two documents", append(append([]byte{}, example...), []byte("\n---\nmode: paper\n")...)},
		{"oversized", bytes.Repeat([]byte("#"), config.MaxConfigBytes+1)},
		{"not a mapping", []byte("- a\n- b\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(bytes.NewReader(tc.input))
			if !errors.Is(err, config.ErrDecode) {
				t.Fatalf("want ErrDecode, got %v", err)
			}
		})
	}
}

func TestLoadFileMissing(t *testing.T) {
	t.Parallel()
	_, err := config.LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

// TestLoadRejectsLooseScalars uses raw text because a Go map cannot express
// YAML 1.1 booleans or untagged floats in integer fields.
func TestLoadRejectsLooseScalars(t *testing.T) {
	t.Parallel()
	example := string(readExample(t))
	tests := []struct{ name, old, new string }{
		{"yes as bool", "secondary_enabled: false", "secondary_enabled: no"},
		{"on as bool", "    enabled: true\n    max_calls_per_day: 40", "    enabled: on\n    max_calls_per_day: 40"},
		{"float literal for int", "max_orders_per_min: 10", "max_orders_per_min: 1e1"},
		{"dot inf", "max_drawdown_pct: 30", "max_drawdown_pct: .inf"},
		{"bool for int", "paper_min_days: 60", "paper_min_days: true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(example, tc.old) {
				t.Fatalf("example does not contain %q", tc.old)
			}
			_, err := config.Load(strings.NewReader(strings.Replace(example, tc.old, tc.new, 1)))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, config.ErrDecode) && !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("unexpected error class: %v", err)
			}
		})
	}
}

func TestLoadDecodeErrorRedactsValues(t *testing.T) {
	t.Parallel()
	example := string(readExample(t))
	for _, repl := range [][2]string{
		{"max_orders_per_min: 10", "max_orders_per_min: sk-SECRET-VALUE"},
		{"price_collar_bps: 50", "price_collar_bps: sk-SECRET-VALUE"},
		{"mode: paper", "mode: [sk-SECRET-VALUE]"},
	} {
		_, err := config.Load(strings.NewReader(strings.Replace(example, repl[0], repl[1], 1)))
		if !errors.Is(err, config.ErrDecode) {
			t.Fatalf("%s: want ErrDecode, got %v", repl[1], err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("decode error leaks the value: %v", err)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLoadFileResolvesRelativeStorePaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, readExample(t), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != filepath.Join(dir, "data", "trader.db") || cfg.Store.BackupDir != filepath.Join(dir, "data", "backups") {
		t.Fatalf("paths not resolved against the config directory: %+v", cfg.Store)
	}
}
