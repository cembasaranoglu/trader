// Package config loads and validates the trader runtime configuration.
//
// The loader is strict by design:
//   - unknown YAML keys are rejected,
//   - every field must be present (there are no defaults in code),
//   - values are range-checked and cross-checked before a Config is returned.
//
// A Config value is only ever produced by Load or LoadFile, so code that holds
// a *Config can rely on it being complete and valid.
package config

import (
	"time"
)

// Mode is the operating mode of the trading process.
type Mode string

// Supported operating modes. Promotion between modes is gated (G1, G2) and is
// never performed by the process itself.
const (
	ModePaper     Mode = "paper"
	ModeLiveSmall Mode = "live_small"
	ModeLiveFull  Mode = "live_full"
)

// OnMaxDrawdown is the position action taken when the max drawdown limit (R-1) is hit.
type OnMaxDrawdown string

// Supported max drawdown actions.
const (
	OnMaxDDCloseAll  OnMaxDrawdown = "close_all"
	OnMaxDDHoldStops OnMaxDrawdown = "hold_stops"
)

// STPMode is the exchange self-trade prevention mode (R-22).
type STPMode string

// Supported self-trade prevention modes. NONE is deliberately not accepted.
const (
	STPExpireTaker STPMode = "EXPIRE_TAKER"
	STPExpireMaker STPMode = "EXPIRE_MAKER"
	STPExpireBoth  STPMode = "EXPIRE_BOTH"
)

// Config is the validated runtime configuration.
type Config struct {
	Mode          Mode
	Venue         Venue
	QuoteCurrency string
	Capital       Capital
	Risk          Risk
	Execution     Execution
	Ops           Ops
	LLM           LLM
	Agents        Agents
	Learning      Learning
	Gates         Gates
}

// Venue selects the exchanges the process may use.
type Venue struct {
	Primary          string
	Secondary        string
	SecondaryEnabled bool
}

// Capital holds capital sizing in USD. The quote currency is USDT; it is
// treated as USD only while the depeg guard (Risk.FXShockPct) passes.
type Capital struct {
	PlannedUSD      float64
	LiveSmallMaxUSD float64
}

// Risk holds the pre-trade risk limits (R-1..R-24).
type Risk struct {
	MaxDrawdownPct          float64
	OnMaxDD                 OnMaxDrawdown
	MaxDailyLossPct         float64
	MaxWeeklyLossPct        float64
	MaxOrderNotionalUSD     float64
	MaxDailyTurnoverPct     float64
	MaxVenueExposurePct     float64
	FXShockPct              float64
	PerTradeRiskPct         float64
	MaxPositionPct          float64
	MaxGrossExposurePct     float64
	PriceCollarBps          float64
	MaxDataAge              time.Duration
	MaxOrdersPerMin         int
	MaxEntriesPerWindow     EntryWindow
	CashReservePct          float64
	MinEdgeToCostRatio      float64
	ReconcileToleranceSteps int
	ReconcileEscalateCycles int
}

// EntryWindow limits repeated entries (R-11).
type EntryWindow struct {
	Count  int
	Window time.Duration
}

// Execution holds order execution settings.
type Execution struct {
	STPMode STPMode
}

// Ops holds operational safety settings.
type Ops struct {
	MaxClockSkew     time.Duration
	HeartbeatTimeout time.Duration
}

// LLM holds model selection and cost caps.
type LLM struct {
	CostCapPctOfCapital float64
	RnDMonthlyCapUSD    float64
	Models              Models
}

// Models names the pinned Claude model IDs.
type Models struct {
	Routine   string
	Synthesis string
}

// Agents holds per-agent switches and call caps.
type Agents struct {
	Events     AgentLimits
	Regime     AgentLimits
	Thesis     ThesisLimits
	Critic     AgentLimits
	Reflection Switch
}

// AgentLimits is an on/off switch with a daily call cap.
type AgentLimits struct {
	Enabled        bool
	MaxCallsPerDay int
}

// ThesisLimits extends AgentLimits with an escalation cap for the larger model.
type ThesisLimits struct {
	AgentLimits
	MaxEscalationsPerDay int
}

// Switch is a plain on/off switch.
type Switch struct {
	Enabled bool
}

// Learning holds the learning-loop safety limits.
type Learning struct {
	MinSupportingTrades int
	MaxParamDeltaPct    float64
}

// Gates holds the autonomy gate thresholds.
type Gates struct {
	PaperMinDays     int
	G2MinDays        int
	EdgeCostMultiple float64
}
