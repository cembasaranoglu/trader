package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxConfigBytes bounds the size of a configuration document.
const MaxConfigBytes = 1 << 20

// Sentinel errors. Every validation problem wraps exactly one of them, so
// callers can use errors.Is to classify a failure.
var (
	// ErrMissing reports a required key that is absent.
	ErrMissing = errors.New("missing required field")
	// ErrInvalid reports a value that is present but not acceptable.
	ErrInvalid = errors.New("invalid value")
	// ErrDecode reports a document that could not be parsed.
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
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open: %w", err)
	}
	defer f.Close()
	return Load(f)
}

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
		return nil, fmt.Errorf("config: %w: %w", ErrDecode, err)
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

// val returns *p, recording ErrMissing and returning the zero value when p is nil.
func val[T any](c *checker, path string, p *T) T {
	var zero T
	if !present(c, path, p) {
		return zero
	}
	return *p
}

// pct returns a percentage in (lo, hi].
func (c *checker) pct(path string, p *float64, lo, hi float64) float64 {
	if p == nil {
		c.missing(path)
		return 0
	}
	if !(*p > lo && *p <= hi) {
		c.invalid(path, "must be in (%g, %g], got %g", lo, hi, *p)
	}
	return *p
}

// reserve returns a percentage in [0, 100).
func (c *checker) reserve(path string, p *float64) float64 {
	if p == nil {
		c.missing(path)
		return 0
	}
	if !(*p >= 0 && *p < 100) {
		c.invalid(path, "must be in [0, 100), got %g", *p)
	}
	return *p
}

// pos returns a strictly positive float.
func (c *checker) pos(path string, p *float64) float64 {
	if p == nil {
		c.missing(path)
		return 0
	}
	if !(*p > 0) {
		c.invalid(path, "must be > 0, got %g", *p)
	}
	return *p
}

// atLeast returns an int >= min.
func (c *checker) atLeast(path string, p *int, min int) int {
	if p == nil {
		c.missing(path)
		return 0
	}
	if *p < min {
		c.invalid(path, "must be >= %d, got %d", min, *p)
	}
	return *p
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

// nonEmpty returns a non-empty string.
func (c *checker) nonEmpty(path string, p *string) string {
	if p == nil {
		c.missing(path)
		return ""
	}
	if *p == "" {
		c.invalid(path, "must not be empty")
	}
	return *p
}

func (c *checker) build(r *rawConfig) *Config {
	cfg := &Config{
		Mode:          Mode(c.oneOf("mode", r.Mode, string(ModePaper), string(ModeLiveSmall), string(ModeLiveFull))),
		QuoteCurrency: c.oneOf("quote_currency", r.QuoteCurrency, "TRY"),
	}
	if present(c, "venue", r.Venue) {
		cfg.Venue = c.venue(r.Venue)
	}
	if present(c, "capital", r.Capital) {
		cfg.Capital = c.capital(r.Capital)
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
			MaxClockSkew:     time.Duration(c.atLeast("ops.max_clock_skew_ms", r.Ops.MaxClockSkewMs, 1)) * time.Millisecond,
			HeartbeatTimeout: time.Duration(c.atLeast("ops.heartbeat_timeout_s", r.Ops.HeartbeatTimeoutS, 1)) * time.Second,
		}
	}
	if present(c, "llm", r.LLM) {
		cfg.LLM = c.llm(r.LLM)
	}
	if present(c, "agents", r.Agents) {
		cfg.Agents = c.agents(r.Agents)
	}
	if present(c, "learning", r.Learning) {
		cfg.Learning = Learning{
			MinSupportingTrades: c.atLeast("learning.min_supporting_trades", r.Learning.MinSupportingTrades, 1),
			MaxParamDeltaPct:    c.pct("learning.max_param_delta_pct", r.Learning.MaxParamDeltaPct, 0, 100),
		}
	}
	if present(c, "gates", r.Gates) {
		cfg.Gates = c.gates(r.Gates)
	}
	c.crossChecks(r)
	return cfg
}

func (c *checker) venue(v *rawVenue) Venue {
	out := Venue{
		Primary:          c.oneOf("venue.primary", v.Primary, "binance_tr"),
		Secondary:        c.oneOf("venue.secondary", v.Secondary, "btcturk"),
		SecondaryEnabled: val(c, "venue.secondary_enabled", v.SecondaryEnabled),
	}
	return out
}

func (c *checker) capital(v *rawCapital) Capital {
	return Capital{
		PlannedUSD:      c.pos("capital.planned_usd", v.PlannedUSD),
		LiveSmallMaxUSD: c.pos("capital.live_small_max_usd", v.LiveSmallMaxUSD),
	}
}

func (c *checker) risk(v *rawRisk) Risk {
	out := Risk{
		MaxDrawdownPct: c.pct("risk.max_drawdown_pct", v.MaxDrawdownPct, 0, 100),
		OnMaxDD: OnMaxDrawdown(c.oneOf("risk.on_max_dd", v.OnMaxDD,
			string(OnMaxDDCloseAll), string(OnMaxDDHoldStops))),
		MaxDailyLossPct:         c.pct("risk.max_daily_loss_pct", v.MaxDailyLossPct, 0, 100),
		MaxWeeklyLossPct:        c.pct("risk.max_weekly_loss_pct", v.MaxWeeklyLossPct, 0, 100),
		MaxOrderNotionalUSD:     c.pos("risk.max_order_notional_usd", v.MaxOrderNotionalUSD),
		MaxDailyTurnoverPct:     c.pos("risk.max_daily_turnover_pct", v.MaxDailyTurnoverPct),
		MaxVenueExposurePct:     c.pct("risk.max_venue_exposure_pct", v.MaxVenueExposurePct, 0, 100),
		FXShockPct:              c.pct("risk.fx_shock_pct", v.FXShockPct, 0, 100),
		PerTradeRiskPct:         c.pct("risk.per_trade_risk_pct", v.PerTradeRiskPct, 0, 100),
		MaxPositionPct:          c.pct("risk.max_position_pct", v.MaxPositionPct, 0, 100),
		MaxGrossExposurePct:     c.pct("risk.max_gross_exposure_pct", v.MaxGrossExposurePct, 0, 100),
		PriceCollarBps:          c.pos("risk.price_collar_bps", v.PriceCollarBps),
		MaxDataAge:              time.Duration(c.atLeast("risk.max_data_age_ms", v.MaxDataAgeMs, 1)) * time.Millisecond,
		MaxOrdersPerMin:         c.atLeast("risk.max_orders_per_min", v.MaxOrdersPerMin, 1),
		CashReservePct:          c.reserve("risk.cash_reserve_pct", v.CashReservePct),
		MinEdgeToCostRatio:      c.pos("risk.min_edge_to_cost_ratio", v.MinEdgeToCostRatio),
		ReconcileToleranceSteps: c.atLeast("risk.reconcile_tolerance_steps", v.ReconcileToleranceSteps, 0),
		ReconcileEscalateCycles: c.atLeast("risk.reconcile_escalate_cycles", v.ReconcileEscalateCycles, 1),
	}
	if out.MinEdgeToCostRatio > 0 && out.MinEdgeToCostRatio < 1 {
		c.invalid("risk.min_edge_to_cost_ratio", "must be >= 1, got %g", out.MinEdgeToCostRatio)
	}
	if present(c, "risk.max_entries_per_window", v.MaxEntriesPerWindow) {
		w := v.MaxEntriesPerWindow
		out.MaxEntriesPerWindow.Count = c.atLeast("risk.max_entries_per_window.count", w.Count, 1)
		if present(c, "risk.max_entries_per_window.window", w.Window) {
			out.MaxEntriesPerWindow.Window = time.Duration(*w.Window)
			if out.MaxEntriesPerWindow.Window <= 0 {
				c.invalid("risk.max_entries_per_window.window", "must be > 0")
			}
		}
	}
	return out
}

func (c *checker) llm(v *rawLLM) LLM {
	out := LLM{
		CostCapPctOfCapital: c.pct("llm.cost_cap_pct_of_capital", v.CostCapPctOfCapital, 0, 100),
		RnDMonthlyCapUSD:    c.pos("llm.rnd_monthly_cap_usd", v.RnDMonthlyCapUSD),
	}
	if present(c, "llm.models", v.Models) {
		out.Models = Models{
			Routine:   c.nonEmpty("llm.models.routine", v.Models.Routine),
			Synthesis: c.nonEmpty("llm.models.synthesis", v.Models.Synthesis),
		}
	}
	return out
}

func (c *checker) agentLimits(path string, v *rawAgentLimits) AgentLimits {
	if !present(c, path, v) {
		return AgentLimits{}
	}
	return AgentLimits{
		Enabled:        val(c, path+".enabled", v.Enabled),
		MaxCallsPerDay: c.atLeast(path+".max_calls_per_day", v.MaxCallsPerDay, 0),
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
				Enabled:        val(c, "agents.thesis.enabled", t.Enabled),
				MaxCallsPerDay: c.atLeast("agents.thesis.max_calls_per_day", t.MaxCallsPerDay, 0),
			},
			MaxEscalationsPerDay: c.atLeast("agents.thesis.max_escalations_per_day", t.MaxEscalationsPerDay, 0),
		}
		if out.Thesis.MaxEscalationsPerDay > out.Thesis.MaxCallsPerDay {
			c.invalid("agents.thesis.max_escalations_per_day", "must be <= agents.thesis.max_calls_per_day (%d), got %d",
				out.Thesis.MaxCallsPerDay, out.Thesis.MaxEscalationsPerDay)
		}
	}
	if present(c, "agents.reflection", v.Reflection) {
		out.Reflection = Switch{Enabled: val(c, "agents.reflection.enabled", v.Reflection.Enabled)}
	}
	return out
}

func (c *checker) gates(v *rawGates) Gates {
	out := Gates{
		PaperMinDays:     c.atLeast("gates.paper_min_days", v.PaperMinDays, 1),
		G2MinDays:        c.atLeast("gates.g2_min_days", v.G2MinDays, 1),
		EdgeCostMultiple: c.pos("gates.edge_cost_multiple", v.EdgeCostMultiple),
	}
	if out.EdgeCostMultiple > 0 && out.EdgeCostMultiple < 1 {
		c.invalid("gates.edge_cost_multiple", "must be >= 1, got %g", out.EdgeCostMultiple)
	}
	return out
}

// minMaxDrawdownPct is the lower bound the user set for the total drawdown limit (D-11).
const minMaxDrawdownPct = 30

// crossChecks enforces relations between fields. It only runs a check when all
// fields it reads were present, so a missing field is reported once.
func (c *checker) crossChecks(r *rawConfig) {
	if rk := r.Risk; rk != nil {
		if rk.MaxDrawdownPct != nil && *rk.MaxDrawdownPct < minMaxDrawdownPct {
			c.invalid("risk.max_drawdown_pct", "must be >= %d (decision D-11), got %g", minMaxDrawdownPct, *rk.MaxDrawdownPct)
		}
		if rk.MaxDailyLossPct != nil && rk.MaxWeeklyLossPct != nil && *rk.MaxDailyLossPct > *rk.MaxWeeklyLossPct {
			c.invalid("risk.max_daily_loss_pct", "must be <= risk.max_weekly_loss_pct")
		}
		if rk.MaxWeeklyLossPct != nil && rk.MaxDrawdownPct != nil && *rk.MaxWeeklyLossPct > *rk.MaxDrawdownPct {
			c.invalid("risk.max_weekly_loss_pct", "must be <= risk.max_drawdown_pct")
		}
		if rk.PerTradeRiskPct != nil && rk.MaxDailyLossPct != nil && *rk.PerTradeRiskPct > *rk.MaxDailyLossPct {
			c.invalid("risk.per_trade_risk_pct", "must be <= risk.max_daily_loss_pct")
		}
		if rk.MaxPositionPct != nil && rk.MaxGrossExposurePct != nil && *rk.MaxPositionPct > *rk.MaxGrossExposurePct {
			c.invalid("risk.max_position_pct", "must be <= risk.max_gross_exposure_pct")
		}
		if rk.MaxOrderNotionalUSD != nil && r.Capital != nil && r.Capital.LiveSmallMaxUSD != nil &&
			*rk.MaxOrderNotionalUSD > *r.Capital.LiveSmallMaxUSD {
			c.invalid("risk.max_order_notional_usd", "must be <= capital.live_small_max_usd")
		}
	}
	if cp := r.Capital; cp != nil && cp.PlannedUSD != nil && cp.LiveSmallMaxUSD != nil && *cp.LiveSmallMaxUSD > *cp.PlannedUSD {
		c.invalid("capital.live_small_max_usd", "must be <= capital.planned_usd")
	}
	if v := r.Venue; v != nil && v.SecondaryEnabled != nil && *v.SecondaryEnabled {
		// The BtcTurk adapter arrives in W9; enabling it before then is a configuration error.
		c.invalid("venue.secondary_enabled", "secondary venue is not supported before wave W9")
	}
}
