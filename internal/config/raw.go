package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// raw* types mirror the YAML document. Every scalar is a pointer so that a
// missing key can be told apart from a zero value.

type rawConfig struct {
	Mode          *string       `yaml:"mode"`
	Venue         *rawVenue     `yaml:"venue"`
	QuoteCurrency *string       `yaml:"quote_currency"`
	Capital       *rawCapital   `yaml:"capital"`
	Risk          *rawRisk      `yaml:"risk"`
	Execution     *rawExecution `yaml:"execution"`
	Ops           *rawOps       `yaml:"ops"`
	LLM           *rawLLM       `yaml:"llm"`
	Agents        *rawAgents    `yaml:"agents"`
	Learning      *rawLearning  `yaml:"learning"`
	Gates         *rawGates     `yaml:"gates"`
}

type rawVenue struct {
	Primary          *string `yaml:"primary"`
	Secondary        *string `yaml:"secondary"`
	SecondaryEnabled *bool   `yaml:"secondary_enabled"`
}

type rawCapital struct {
	PlannedUSD      *float64 `yaml:"planned_usd"`
	LiveSmallMaxUSD *float64 `yaml:"live_small_max_usd"`
}

type rawRisk struct {
	MaxDrawdownPct          *float64        `yaml:"max_drawdown_pct"`
	OnMaxDD                 *string         `yaml:"on_max_dd"`
	MaxDailyLossPct         *float64        `yaml:"max_daily_loss_pct"`
	MaxWeeklyLossPct        *float64        `yaml:"max_weekly_loss_pct"`
	MaxOrderNotionalUSD     *float64        `yaml:"max_order_notional_usd"`
	MaxDailyTurnoverPct     *float64        `yaml:"max_daily_turnover_pct"`
	MaxVenueExposurePct     *float64        `yaml:"max_venue_exposure_pct"`
	FXShockPct              *float64        `yaml:"fx_shock_pct"`
	PerTradeRiskPct         *float64        `yaml:"per_trade_risk_pct"`
	MaxPositionPct          *float64        `yaml:"max_position_pct"`
	MaxGrossExposurePct     *float64        `yaml:"max_gross_exposure_pct"`
	PriceCollarBps          *float64        `yaml:"price_collar_bps"`
	MaxDataAgeMs            *int            `yaml:"max_data_age_ms"`
	MaxOrdersPerMin         *int            `yaml:"max_orders_per_min"`
	MaxEntriesPerWindow     *rawEntryWindow `yaml:"max_entries_per_window"`
	CashReservePct          *float64        `yaml:"cash_reserve_pct"`
	MinEdgeToCostRatio      *float64        `yaml:"min_edge_to_cost_ratio"`
	ReconcileToleranceSteps *int            `yaml:"reconcile_tolerance_steps"`
	ReconcileEscalateCycles *int            `yaml:"reconcile_escalate_cycles"`
}

type rawEntryWindow struct {
	Count  *int          `yaml:"count"`
	Window *yamlDuration `yaml:"window"`
}

type rawExecution struct {
	STPMode *string `yaml:"stp_mode"`
}

type rawOps struct {
	MaxClockSkewMs    *int `yaml:"max_clock_skew_ms"`
	HeartbeatTimeoutS *int `yaml:"heartbeat_timeout_s"`
}

type rawLLM struct {
	CostCapPctOfCapital *float64   `yaml:"cost_cap_pct_of_capital"`
	RnDMonthlyCapUSD    *float64   `yaml:"rnd_monthly_cap_usd"`
	Models              *rawModels `yaml:"models"`
}

type rawModels struct {
	Routine   *string `yaml:"routine"`
	Synthesis *string `yaml:"synthesis"`
}

type rawAgents struct {
	Events     *rawAgentLimits  `yaml:"events"`
	Regime     *rawAgentLimits  `yaml:"regime"`
	Thesis     *rawThesisLimits `yaml:"thesis"`
	Critic     *rawAgentLimits  `yaml:"critic"`
	Reflection *rawSwitch       `yaml:"reflection"`
}

type rawAgentLimits struct {
	Enabled        *bool `yaml:"enabled"`
	MaxCallsPerDay *int  `yaml:"max_calls_per_day"`
}

type rawThesisLimits struct {
	Enabled              *bool `yaml:"enabled"`
	MaxCallsPerDay       *int  `yaml:"max_calls_per_day"`
	MaxEscalationsPerDay *int  `yaml:"max_escalations_per_day"`
}

type rawSwitch struct {
	Enabled *bool `yaml:"enabled"`
}

type rawLearning struct {
	MinSupportingTrades *int     `yaml:"min_supporting_trades"`
	MaxParamDeltaPct    *float64 `yaml:"max_param_delta_pct"`
}

type rawGates struct {
	PaperMinDays     *int     `yaml:"paper_min_days"`
	G2MinDays        *int     `yaml:"g2_min_days"`
	EdgeCostMultiple *float64 `yaml:"edge_cost_multiple"`
}

// yamlDuration accepts Go duration strings such as "60m" or "1h30m".
// Bare numbers are rejected so that a unit is always explicit.
type yamlDuration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *yamlDuration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line %d: duration must be a string with a unit, e.g. \"60m\"", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = yamlDuration(v)
	return nil
}
