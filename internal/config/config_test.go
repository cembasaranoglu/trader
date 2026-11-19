package config_test

import (
	"bytes"
	"errors"
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
		{"venue.primary", cfg.Venue.Primary, "binance_tr"},
		{"quote", cfg.QuoteCurrency, "TRY"},
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
		{"unsupported venue", func(t *testing.T, m map[string]any) { set(t, m, "venue.primary", "binance") }, config.ErrInvalid, "venue.primary"},
		{"secondary venue before W9", func(t *testing.T, m map[string]any) { set(t, m, "venue.secondary_enabled", true) }, config.ErrInvalid, "venue.secondary_enabled"},
		{"non-TRY quote", func(t *testing.T, m map[string]any) { set(t, m, "quote_currency", "USDT") }, config.ErrInvalid, "quote_currency"},
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
