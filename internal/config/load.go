package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxConfigBytes bounds the size of a configuration document.
const MaxConfigBytes = 1 << 20

// Sentinel errors. Every problem wraps exactly one of them, so callers can
// use errors.Is to classify a failure.
var (
	// ErrMissing reports a required key that is absent.
	ErrMissing = errors.New("missing required field")
	// ErrInvalid reports a value that is present but not acceptable.
	ErrInvalid = errors.New("invalid value")
	// ErrDecode reports a document that could not be parsed or has a wrongly
	// typed value. Decode errors are reported before field validation runs.
	ErrDecode = errors.New("decode failed")
)

// FieldError describes one problem with one configuration field.
type FieldError struct {
	Path string // dotted YAML path, e.g. "risk.max_drawdown_pct"
	Msg  string // human-readable detail; empty for ErrMissing
	Err  error  // ErrMissing or ErrInvalid
}

// Error implements error.
func (e *FieldError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%s: %v", e.Path, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Path, e.Err, e.Msg)
}

// Unwrap returns the sentinel error.
func (e *FieldError) Unwrap() error { return e.Err }

// LoadFile reads and validates the configuration file at path.
//
// Relative store paths are resolved against the directory of the config
// file, not the process working directory (which is "/" under systemd by
// default), so the same file always names the same database.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open: %w", err)
	}
	defer f.Close()
	cfg, err := Load(f)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config: resolve directory: %w", err)
	}
	for _, p := range []*string{&cfg.Store.DSNFile, &cfg.Store.MigrateDSNFile, &cfg.Store.AnchorPath, &cfg.Store.ExportDir} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return cfg, nil
}

// quotedValue matches the backtick-quoted values that yaml.v3 puts into type
// errors; they are redacted so a misplaced secret is never echoed.
var quotedValue = regexp.MustCompile("`[^`]*`")

// Load reads a single YAML document from r, rejects unknown keys and returns a
// validated Config. When validation fails the returned error joins one
// *FieldError per problem (see errors.Join).
func Load(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("config: %w: document larger than %d bytes", ErrDecode, MaxConfigBytes)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: %w: empty document", ErrDecode)
		}
		return nil, fmt.Errorf("config: %w: %s", ErrDecode, quotedValue.ReplaceAllString(err.Error(), "`<redacted>`"))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w: more than one YAML document", ErrDecode)
	}

	c := &checker{}
	cfg := c.build(&raw)
	if err := c.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checker accumulates field errors while building a Config from raw input.
type checker struct {
	errs []error
}

func (c *checker) err() error {
	if len(c.errs) == 0 {
		return nil
	}
	return errors.Join(c.errs...)
}

func (c *checker) missing(path string) {
	c.errs = append(c.errs, &FieldError{Path: path, Err: ErrMissing})
}

func (c *checker) invalid(path, format string, args ...any) {
	c.errs = append(c.errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...), Err: ErrInvalid})
}

// present reports whether p is non-nil and records ErrMissing otherwise.
func present[T any](c *checker, path string, p *T) bool {
	if p == nil {
		c.missing(path)
		return false
	}
	return true
}

// num returns a finite float in the interval described by lo/hi and the
// inclusivity flags. NaN and ±Inf are always rejected.
func (c *checker) num(path string, p *float64, lo float64, loIncl bool, hi float64, hiIncl bool) float64 {
	if p == nil {
		c.missing(path)
		return 0
	}
	v := *p
	ok := !math.IsNaN(v) && !math.IsInf(v, 0) &&
		(v > lo || (loIncl && v == lo)) && (v < hi || (hiIncl && v == hi))
	if !ok {
		l, h := "(", ")"
		if loIncl {
			l = "["
		}
		if hiIncl {
			h = "]"
		}
		c.invalid(path, "must be a finite number in %s%g, %g%s, got %g", l, lo, hi, h, v)
	}
	return v
}

// pct returns a percentage in (0, 100].
func (c *checker) pct(path string, p *float64) float64 {
	return c.num(path, p, 0, false, 100, true)
}

// pos returns a finite float in (0, max].
func (c *checker) pos(path string, p *float64, max float64) float64 {
	return c.num(path, p, 0, false, max, true)
}

// intIn returns an integer in [min, max].
func (c *checker) intIn(path string, p *yamlInt, min, max int) int {
	if p == nil {
		c.missing(path)
		return 0
	}
	v := int(*p)
	if v < min || v > max {
		c.invalid(path, "must be an integer in [%d, %d], got %d", min, max, v)
	}
	return v
}

// boolean returns a required boolean.
func (c *checker) boolean(path string, p *yamlBool) bool {
	if p == nil {
		c.missing(path)
		return false
	}
	return bool(*p)
}

// oneOf returns a string that must be one of allowed.
func (c *checker) oneOf(path string, p *string, allowed ...string) string {
	if p == nil {
		c.missing(path)
		return ""
	}
	for _, a := range allowed {
		if *p == a {
			return *p
		}
	}
	c.invalid(path, "must be one of %q, got %q", allowed, *p)
	return *p
}

// nonBlank returns a string that is not empty or whitespace-only.
func (c *checker) nonBlank(path string, p *string) string {
	if p == nil {
		c.missing(path)
		return ""
	}
	if strings.TrimSpace(*p) == "" || strings.TrimSpace(*p) != *p {
		c.invalid(path, "must be non-empty without surrounding whitespace")
	}
	return *p
}

// filePath returns a non-blank, cleaned path without NUL bytes.
func (c *checker) filePath(path string, p *string) string {
	v := c.nonBlank(path, p)
	if v == "" {
		return ""
	}
	if strings.ContainsRune(v, 0) || len(v) > 4096 {
		c.invalid(path, "must be a valid path")
		return ""
	}
	return filepath.Clean(v)
}

// Sanity bounds. They prevent overflow and nonsensical values; they are not
// risk policy (risk values come from D-16 in the example config).
const (
	maxUSD        = 1e9
	maxBps        = 10000 // 100%
	maxTurnover   = 10000 // 100x equity per day
	maxRatio      = 1000
	maxCount      = 100000
	maxDays       = 3650
	maxSkewMs     = 60000 // Binance recvWindow upper limit
	maxDataAgeMs  = 60000
	maxHeartbeatS = 3600
	minWindow     = time.Minute
	maxWindow     = 24 * time.Hour
)

// appRolePattern is a PostgreSQL role name the store accepts.
var appRolePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// minMaxDrawdownPct is the lower bound the user set for the total drawdown limit (D-11).
const minMaxDrawdownPct = 30

func (c *checker) build(r *rawConfig) *Config {
	cfg := &Config{
		Mode:          Mode(c.oneOf("mode", r.Mode, string(ModePaper), string(ModeLiveSmall), string(ModeLiveFull))),
		QuoteCurrency: c.oneOf("quote_currency", r.QuoteCurrency, "USDT"),
	}
	if present(c, "venue", r.Venue) {
		cfg.Venue = Venue{
			// D-17: binance.com is the only primary venue the trading stack
			// implements. The Binance TR client exists only for venuecheck.
			Primary:          c.oneOf("venue.primary", r.Venue.Primary, "binance"),
			Secondary:        c.oneOf("venue.secondary", r.Venue.Secondary, "btcturk"),
			SecondaryEnabled: c.boolean("venue.secondary_enabled", r.Venue.SecondaryEnabled),
		}
	}
	if present(c, "capital", r.Capital) {
		cfg.Capital = Capital{
			PlannedUSD:      c.pos("capital.planned_usd", r.Capital.PlannedUSD, maxUSD),
			LiveSmallMaxUSD: c.pos("capital.live_small_max_usd", r.Capital.LiveSmallMaxUSD, maxUSD),
		}
	}
	if present(c, "risk", r.Risk) {
		cfg.Risk = c.risk(r.Risk)
	}
	if present(c, "execution", r.Execution) {
		cfg.Execution = Execution{
			STPMode: STPMode(c.oneOf("execution.stp_mode", r.Execution.STPMode,
				string(STPExpireTaker), string(STPExpireMaker), string(STPExpireBoth))),
		}
	}
	if present(c, "ops", r.Ops) {
		cfg.Ops = Ops{
			MaxClockSkew:     time.Duration(c.intIn("ops.max_clock_skew_ms", r.Ops.MaxClockSkewMs, 1, maxSkewMs)) * time.Millisecond,
			HeartbeatTimeout: time.Duration(c.intIn("ops.heartbeat_timeout_s", r.Ops.HeartbeatTimeoutS, 1, maxHeartbeatS)) * time.Second,
		}
	}
	if present(c, "marketdata", r.MarketData) {
		md := r.MarketData
		cfg.MarketData = MarketData{
			DepthLimit:          c.intIn("marketdata.depth_limit", md.DepthLimit, 1, 5000),
			ReconnectMaxBackoff: time.Duration(c.intIn("marketdata.reconnect_max_backoff_s", md.ReconnectMaxBackoff, 1, 300)) * time.Second,
		}
		if present(c, "marketdata.kline_intervals", md.KlineIntervals) {
			ivs := *md.KlineIntervals
			if len(ivs) == 0 {
				c.invalid("marketdata.kline_intervals", "must list at least one interval")
			}
			seen := map[string]bool{}
			for _, iv := range ivs {
				if !slices.Contains(KlineIntervals, iv) {
					c.invalid("marketdata.kline_intervals", "unsupported interval %q", iv)
				} else if seen[iv] {
					c.invalid("marketdata.kline_intervals", "duplicate interval %q", iv)
				}
				seen[iv] = true
			}
			cfg.MarketData.KlineIntervals = slices.Clone(ivs)
		}
	}
	if present(c, "store", r.Store) {
		st := r.Store
		cfg.Store = Store{
			DSNFile:    c.filePath("store.dsn_file", st.DSNFile),
			AppRole:    c.nonBlank("store.app_role", st.AppRole),
			AnchorPath: c.filePath("store.anchor_path", st.AnchorPath),
			ExportDir:  c.filePath("store.export_dir", st.ExportDir),
		}
		if st.MigrateDSNFile != nil {
			cfg.Store.MigrateDSNFile = c.filePath("store.migrate_dsn_file", st.MigrateDSNFile)
		}
		if cfg.Store.AppRole != "" && !appRolePattern.MatchString(cfg.Store.AppRole) {
			c.invalid("store.app_role", "must match %s", appRolePattern)
		}
		if cfg.Store.DSNFile != "" && cfg.Store.DSNFile == cfg.Store.MigrateDSNFile {
			c.invalid("store.migrate_dsn_file", "must differ from store.dsn_file (owner and application roles are separate)")
		}
		if cfg.Store.AnchorPath != "" && cfg.Store.ExportDir != "" {
			an, _ := filepath.Abs(cfg.Store.AnchorPath)
			ex, _ := filepath.Abs(cfg.Store.ExportDir)
			if an == ex || strings.HasPrefix(an, ex+string(filepath.Separator)) {
				c.invalid("store.export_dir", "must not contain the anchor file")
			}
		}
	}
	if present(c, "llm", r.LLM) {
		cfg.LLM = LLM{
			CostCapPctOfCapital: c.pct("llm.cost_cap_pct_of_capital", r.LLM.CostCapPctOfCapital),
			RnDMonthlyCapUSD:    c.pos("llm.rnd_monthly_cap_usd", r.LLM.RnDMonthlyCapUSD, maxUSD),
		}
		if present(c, "llm.models", r.LLM.Models) {
			cfg.LLM.Models = Models{
				Routine:   c.nonBlank("llm.models.routine", r.LLM.Models.Routine),
				Synthesis: c.nonBlank("llm.models.synthesis", r.LLM.Models.Synthesis),
			}
		}
	}
	if present(c, "agents", r.Agents) {
		cfg.Agents = c.agents(r.Agents)
	}
	if present(c, "learning", r.Learning) {
		cfg.Learning = Learning{
			MinSupportingTrades: c.intIn("learning.min_supporting_trades", r.Learning.MinSupportingTrades, 1, maxCount),
			MaxParamDeltaPct:    c.pct("learning.max_param_delta_pct", r.Learning.MaxParamDeltaPct),
		}
	}
	if present(c, "gates", r.Gates) {
		cfg.Gates = Gates{
			PaperMinDays:     c.intIn("gates.paper_min_days", r.Gates.PaperMinDays, 1, maxDays),
			G2MinDays:        c.intIn("gates.g2_min_days", r.Gates.G2MinDays, 1, maxDays),
			EdgeCostMultiple: c.num("gates.edge_cost_multiple", r.Gates.EdgeCostMultiple, 1, true, maxRatio, true),
			InfraMonthlyUSD:  c.num("gates.infra_monthly_usd", r.Gates.InfraMonthlyUSD, 0, true, maxUSD, true),
		}
	}
	c.crossChecks(r)
	return cfg
}

func (c *checker) risk(v *rawRisk) Risk {
	out := Risk{
		MaxDrawdownPct: c.pct("risk.max_drawdown_pct", v.MaxDrawdownPct),
		OnMaxDD: OnMaxDrawdown(c.oneOf("risk.on_max_dd", v.OnMaxDD,
			string(OnMaxDDCloseAll), string(OnMaxDDHoldStops))),
		MaxDailyLossPct:         c.pct("risk.max_daily_loss_pct", v.MaxDailyLossPct),
		MaxWeeklyLossPct:        c.pct("risk.max_weekly_loss_pct", v.MaxWeeklyLossPct),
		MaxOrderNotionalUSD:     c.pos("risk.max_order_notional_usd", v.MaxOrderNotionalUSD, maxUSD),
		MaxDailyTurnoverPct:     c.pos("risk.max_daily_turnover_pct", v.MaxDailyTurnoverPct, maxTurnover),
		MaxVenueExposurePct:     c.pct("risk.max_venue_exposure_pct", v.MaxVenueExposurePct),
		FXShockPct:              c.pct("risk.fx_shock_pct", v.FXShockPct),
		PerTradeRiskPct:         c.pct("risk.per_trade_risk_pct", v.PerTradeRiskPct),
		MaxPositionPct:          c.pct("risk.max_position_pct", v.MaxPositionPct),
		MaxGrossExposurePct:     c.pct("risk.max_gross_exposure_pct", v.MaxGrossExposurePct),
		PriceCollarBps:          c.pos("risk.price_collar_bps", v.PriceCollarBps, maxBps),
		MaxDataAge:              time.Duration(c.intIn("risk.max_data_age_ms", v.MaxDataAgeMs, 1, maxDataAgeMs)) * time.Millisecond,
		MaxOrdersPerMin:         c.intIn("risk.max_orders_per_min", v.MaxOrdersPerMin, 1, maxCount),
		CashReservePct:          c.num("risk.cash_reserve_pct", v.CashReservePct, 0, true, 100, false),
		MinEdgeToCostRatio:      c.num("risk.min_edge_to_cost_ratio", v.MinEdgeToCostRatio, 1, true, maxRatio, true),
		ReconcileToleranceSteps: c.intIn("risk.reconcile_tolerance_steps", v.ReconcileToleranceSteps, 0, maxCount),
		ReconcileEscalateCycles: c.intIn("risk.reconcile_escalate_cycles", v.ReconcileEscalateCycles, 1, maxCount),
	}
	if present(c, "risk.max_entries_per_window", v.MaxEntriesPerWindow) {
		w := v.MaxEntriesPerWindow
		out.MaxEntriesPerWindow.Count = c.intIn("risk.max_entries_per_window.count", w.Count, 1, maxCount)
		if present(c, "risk.max_entries_per_window.window", w.Window) {
			d := time.Duration(*w.Window)
			if d < minWindow || d > maxWindow {
				c.invalid("risk.max_entries_per_window.window", "must be in [%s, %s], got %s", minWindow, maxWindow, d)
			}
			out.MaxEntriesPerWindow.Window = d
		}
	}
	return out
}

func (c *checker) agentLimits(path string, v *rawAgentLimits) AgentLimits {
	if !present(c, path, v) {
		return AgentLimits{}
	}
	return AgentLimits{
		Enabled:        c.boolean(path+".enabled", v.Enabled),
		MaxCallsPerDay: c.intIn(path+".max_calls_per_day", v.MaxCallsPerDay, 0, maxCount),
	}
}

func (c *checker) agents(v *rawAgents) Agents {
	out := Agents{
		Events: c.agentLimits("agents.events", v.Events),
		Regime: c.agentLimits("agents.regime", v.Regime),
		Critic: c.agentLimits("agents.critic", v.Critic),
	}
	if present(c, "agents.thesis", v.Thesis) {
		t := v.Thesis
		out.Thesis = ThesisLimits{
			AgentLimits: AgentLimits{
				Enabled:        c.boolean("agents.thesis.enabled", t.Enabled),
				MaxCallsPerDay: c.intIn("agents.thesis.max_calls_per_day", t.MaxCallsPerDay, 0, maxCount),
			},
			MaxEscalationsPerDay: c.intIn("agents.thesis.max_escalations_per_day", t.MaxEscalationsPerDay, 0, maxCount),
		}
		if t.MaxCallsPerDay != nil && t.MaxEscalationsPerDay != nil && *t.MaxEscalationsPerDay > *t.MaxCallsPerDay {
			c.invalid("agents.thesis.max_escalations_per_day", "must be <= agents.thesis.max_calls_per_day (%d), got %d",
				*t.MaxCallsPerDay, *t.MaxEscalationsPerDay)
		}
	}
	if present(c, "agents.reflection", v.Reflection) {
		out.Reflection = Switch{Enabled: c.boolean("agents.reflection.enabled", v.Reflection.Enabled)}
	}
	return out
}

// finite reports whether p holds a finite value; cross checks skip
// non-finite inputs because the field check already reported them.
func finite(p *float64) bool { return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) }

// crossChecks enforces relations between fields. A check only runs when all
// fields it reads are present and finite, so each problem is reported once.
func (c *checker) crossChecks(r *rawConfig) {
	if rk := r.Risk; rk != nil {
		if finite(rk.MaxDrawdownPct) && *rk.MaxDrawdownPct < minMaxDrawdownPct {
			c.invalid("risk.max_drawdown_pct", "must be >= %d (decision D-11), got %g", minMaxDrawdownPct, *rk.MaxDrawdownPct)
		}
		if finite(rk.MaxDailyLossPct) && finite(rk.MaxWeeklyLossPct) && *rk.MaxDailyLossPct > *rk.MaxWeeklyLossPct {
			c.invalid("risk.max_daily_loss_pct", "must be <= risk.max_weekly_loss_pct")
		}
		if finite(rk.MaxWeeklyLossPct) && finite(rk.MaxDrawdownPct) && *rk.MaxWeeklyLossPct > *rk.MaxDrawdownPct {
			c.invalid("risk.max_weekly_loss_pct", "must be <= risk.max_drawdown_pct")
		}
		if finite(rk.PerTradeRiskPct) && finite(rk.MaxDailyLossPct) && *rk.PerTradeRiskPct > *rk.MaxDailyLossPct {
			c.invalid("risk.per_trade_risk_pct", "must be <= risk.max_daily_loss_pct")
		}
		if finite(rk.MaxPositionPct) && finite(rk.MaxGrossExposurePct) && *rk.MaxPositionPct > *rk.MaxGrossExposurePct {
			c.invalid("risk.max_position_pct", "must be <= risk.max_gross_exposure_pct")
		}
		if finite(rk.MaxGrossExposurePct) && finite(rk.CashReservePct) && *rk.MaxGrossExposurePct+*rk.CashReservePct > 100 {
			c.invalid("risk.max_gross_exposure_pct", "plus risk.cash_reserve_pct must be <= 100")
		}
		if finite(rk.MaxOrderNotionalUSD) && r.Capital != nil && finite(r.Capital.LiveSmallMaxUSD) &&
			*rk.MaxOrderNotionalUSD > *r.Capital.LiveSmallMaxUSD {
			c.invalid("risk.max_order_notional_usd", "must be <= capital.live_small_max_usd")
		}
	}
	if cp := r.Capital; cp != nil && finite(cp.PlannedUSD) && finite(cp.LiveSmallMaxUSD) && *cp.LiveSmallMaxUSD > *cp.PlannedUSD {
		c.invalid("capital.live_small_max_usd", "must be <= capital.planned_usd")
	}
	if r.Mode != nil && (*r.Mode == string(ModeLiveSmall) || *r.Mode == string(ModeLiveFull)) {
		// Live trading is not implemented; it is enabled in W8 after gate G1 (ADR-004).
		c.invalid("mode", "%s is not supported yet; only paper", *r.Mode)
	}
	if v := r.Venue; v != nil && v.SecondaryEnabled != nil && bool(*v.SecondaryEnabled) {
		// The BtcTurk adapter is not implemented; enabling it is a configuration error.
		c.invalid("venue.secondary_enabled", "secondary venue is not supported yet")
	}
}
