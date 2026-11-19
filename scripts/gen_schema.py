#!/usr/bin/env python3
"""Generate configs/schema.json from configs/example.yaml.

Usage: python3 scripts/gen_schema.py configs/example.yaml > configs/schema.json
Requires PyYAML. The bounds below mirror internal/config/load.go; the Go loader
is the enforcing validator, this schema only helps editors.
"""
import json, sys, yaml
ex = yaml.safe_load(open(sys.argv[1]))
pct = {"type": "number", "exclusiveMinimum": 0, "maximum": 100}
usd = {"type": "number", "exclusiveMinimum": 0, "maximum": 1e9}
C = {
 "mode": {"enum": ["paper", "live_small", "live_full"]},
 "venue.primary": {"enum": ["binance"], "description": "D-17: binance.com"},
 "venue.secondary": {"enum": ["btcturk"]},
 "venue.secondary_enabled": {"type": "boolean", "const": False, "description": "BtcTurk adapter arrives in W9"},
 "quote_currency": {"enum": ["USDT"]},
 "capital.planned_usd": usd, "capital.live_small_max_usd": usd,
 "risk.max_drawdown_pct": {"type": "number", "minimum": 30, "maximum": 100, "description": "D-11"},
 "risk.on_max_dd": {"enum": ["close_all", "hold_stops"]},
 "risk.max_daily_loss_pct": pct, "risk.max_weekly_loss_pct": pct,
 "risk.max_order_notional_usd": usd, "risk.max_daily_turnover_pct": {"type": "number", "exclusiveMinimum": 0, "maximum": 10000},
 "risk.max_venue_exposure_pct": pct, "risk.fx_shock_pct": pct, "risk.per_trade_risk_pct": pct,
 "risk.max_position_pct": pct, "risk.max_gross_exposure_pct": pct, "risk.price_collar_bps": {"type": "number", "exclusiveMinimum": 0, "maximum": 10000},
 "risk.max_data_age_ms": {"type": "integer", "minimum": 1, "maximum": 60000},
 "risk.max_orders_per_min": {"type": "integer", "minimum": 1, "maximum": 100000},
 "risk.max_entries_per_window.count": {"type": "integer", "minimum": 1, "maximum": 100000},
 "risk.max_entries_per_window.window": {"type": "string", "pattern": "^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$", "description": "Go duration between 1m and 24h, e.g. 60m"},
 "risk.cash_reserve_pct": {"type": "number", "minimum": 0, "exclusiveMaximum": 100},
 "risk.min_edge_to_cost_ratio": {"type": "number", "minimum": 1, "maximum": 1000},
 "risk.reconcile_tolerance_steps": {"type": "integer", "minimum": 0, "maximum": 100000},
 "risk.reconcile_escalate_cycles": {"type": "integer", "minimum": 1, "maximum": 100000},
 "execution.stp_mode": {"enum": ["EXPIRE_TAKER", "EXPIRE_MAKER", "EXPIRE_BOTH"]},
 "ops.max_clock_skew_ms": {"type": "integer", "minimum": 1, "maximum": 60000},
 "ops.heartbeat_timeout_s": {"type": "integer", "minimum": 1, "maximum": 3600},
 "marketdata.depth_limit": {"type": "integer", "minimum": 1, "maximum": 5000},
 "marketdata.kline_intervals": {"type": "array", "minItems": 1, "uniqueItems": True,
     "items": {"enum": ["1s", "1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d", "3d", "1w", "1M"]}},
 "marketdata.reconnect_max_backoff_s": {"type": "integer", "minimum": 1, "maximum": 300},
 "llm.cost_cap_pct_of_capital": pct, "llm.rnd_monthly_cap_usd": usd,
 "llm.models.routine": {"type": "string", "pattern": "^\\S(.*\\S)?$"}, "llm.models.synthesis": {"type": "string", "pattern": "^\\S(.*\\S)?$"},
 "learning.min_supporting_trades": {"type": "integer", "minimum": 1, "maximum": 100000},
 "learning.max_param_delta_pct": pct,
 "gates.paper_min_days": {"type": "integer", "minimum": 1, "maximum": 3650},
 "gates.g2_min_days": {"type": "integer", "minimum": 1, "maximum": 3650},
 "gates.edge_cost_multiple": {"type": "number", "minimum": 1, "maximum": 1000},
}
def node(v, path):
    if isinstance(v, dict):
        return {"type": "object", "additionalProperties": False, "required": list(v.keys()),
                "properties": {k: node(x, f"{path}.{k}" if path else k) for k, x in v.items()}}
    if path in C: return dict(C[path])
    if path.endswith(".enabled"): return {"type": "boolean"}
    if path.endswith("max_calls_per_day") or path.endswith("max_escalations_per_day"): return {"type": "integer", "minimum": 0, "maximum": 100000}
    raise SystemExit(f"no constraint for {path}")
s = node(ex, "")
s = {"$schema": "https://json-schema.org/draft/2020-12/schema",
     "$id": "https://github.com/cembasaranoglu/trader/configs/schema.json",
     "title": "trader configuration",
     "description": "Editor-side schema. The Go loader (internal/config) is the enforcing validator and also checks cross-field rules that JSON Schema cannot express.",
     **s}
print(json.dumps(s, indent=2, ensure_ascii=False))
