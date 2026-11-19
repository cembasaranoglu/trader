// Package preflight runs the safety checks that must pass before the trading
// process starts:
//
//   - clock (R-20): |server − local| plus half the round trip must stay within
//     ops.max_clock_skew_ms, so the bound holds even with a slow network;
//   - API key permissions (I-7): withdrawals off, no permission outside
//     reading and spot trading, and trading only with an IP restriction;
//   - self-trade prevention (R-22): the configured STP mode is allowed on
//     every symbol, and every symbol exists;
//   - event log integrity: the hash chain verifies against its anchor.
//
// Every check runs and is reported; the run fails when any check fails. The
// permission check is skipped only for the Demo environment, where the /sapi
// endpoint is not known to be served; a failing call never causes a skip.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

// ErrRefused is returned when at least one check failed; the returned error
// also wraps the specific reasons below (errors.Is works for each).
var ErrRefused = errors.New("preflight: refused")

// Refusal reasons.
var (
	ErrClockSkew          = errors.New("preflight: clock skew above the limit")
	ErrServerTime         = errors.New("preflight: server time unavailable")
	ErrPermissions        = errors.New("preflight: API key permissions unavailable")
	ErrWithdrawalsEnabled = errors.New("preflight: API key has withdrawals enabled")
	ErrReadingDisabled    = errors.New("preflight: API key cannot read")
	ErrNoIPRestriction    = errors.New("preflight: API key can trade without an IP restriction")
	ErrExtraPermissions   = errors.New("preflight: API key has extra permissions")
	ErrExchangeInfo       = errors.New("preflight: exchangeInfo unavailable")
	ErrSymbolMissing      = errors.New("preflight: symbol not found")
	ErrSTPNotAllowed      = errors.New("preflight: STP mode not allowed for a symbol")
	ErrSymbolNotTrading   = errors.New("preflight: symbol not trading")
	ErrEventLog           = errors.New("preflight: event log invalid")
	ErrDatabaseMissing    = errors.New("preflight: database missing while its anchor exists")
)

// Environments.
const (
	EnvLive = "live"
	EnvDemo = "demo"
)

// Exchange is the exchange surface the checks need.
type Exchange interface {
	SyncTime(ctx context.Context) (time.Duration, error)
	KeyPermissions(ctx context.Context) (exchange.KeyPermissions, error)
	Symbols(ctx context.Context, names ...string) ([]exchange.Symbol, error)
}

// ChainVerifier verifies a database file and its anchor (store.VerifyFile).
type ChainVerifier func(ctx context.Context, path string) error

// Params configures Run.
type Params struct {
	Exchange     Exchange
	Environment  string // EnvLive or EnvDemo
	Symbols      []string
	STPMode      string
	MaxClockSkew time.Duration
	StorePath    string
	VerifyChain  ChainVerifier
	Now          func() time.Time
}

// Check is the outcome of one check.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
	Err     error  `json:"-"` // joined refusal reasons when the check failed
}

// Report lists every check in order.
type Report struct {
	Environment string  `json:"environment"`
	Checks      []Check `json:"checks"`
	OK          bool    `json:"ok"`
}

// Run executes all checks. It returns an error wrapping ErrRefused when any
// check failed, and an error for invalid parameters.
func Run(ctx context.Context, p Params) (Report, error) {
	if p.Exchange == nil || p.VerifyChain == nil || p.StorePath == "" || p.MaxClockSkew <= 0 || p.STPMode == "" {
		return Report{}, errors.New("preflight: incomplete parameters")
	}
	if p.Environment != EnvLive && p.Environment != EnvDemo {
		return Report{}, fmt.Errorf("preflight: unknown environment %q", p.Environment)
	}
	if len(p.Symbols) == 0 {
		return Report{}, errors.New("preflight: no symbols")
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	rep := Report{Environment: p.Environment, OK: true}
	add := func(c Check) {
		rep.Checks = append(rep.Checks, c)
		if !c.OK && !c.Skipped {
			rep.OK = false
		}
	}
	add(checkClock(ctx, p, now))
	if p.Environment == EnvDemo {
		add(Check{Name: "key_permissions", Skipped: true, OK: true,
			Detail: "demo environment: /sapi/v1/account/apiRestrictions is not verified there; the live key is checked before live trading"})
	} else {
		add(checkPermissions(ctx, p.Exchange))
	}
	add(checkSymbols(ctx, p))
	add(checkChain(ctx, p))
	if !rep.OK {
		var failed []string
		errs := []error{ErrRefused}
		for _, c := range rep.Checks {
			if !c.OK && !c.Skipped {
				failed = append(failed, c.Name)
				errs = append(errs, c.Err)
			}
		}
		return rep, fmt.Errorf("%w (%s)", errors.Join(errs...), strings.Join(failed, ", "))
	}
	return rep, nil
}

func checkClock(ctx context.Context, p Params, now func() time.Time) Check {
	c := Check{Name: "clock"}
	start := now()
	off, err := p.Exchange.SyncTime(ctx)
	rtt := now().Sub(start)
	if err != nil {
		c.Detail = "server time: " + err.Error()
		c.Err = fmt.Errorf("%w: %v", ErrServerTime, err)
		return c
	}
	abs := off
	if abs < 0 {
		abs = -abs
	}
	bound := abs + rtt/2
	c.Detail = fmt.Sprintf("offset %s, round trip %s, worst case %s, limit %s", off, rtt, bound, p.MaxClockSkew)
	c.OK = bound <= p.MaxClockSkew
	if !c.OK {
		c.Err = ErrClockSkew
	}
	return c
}

func checkPermissions(ctx context.Context, ex Exchange) Check {
	c := Check{Name: "key_permissions"}
	perm, err := ex.KeyPermissions(ctx)
	if err != nil {
		c.Detail = "read permissions: " + err.Error()
		c.Err = fmt.Errorf("%w: %v", ErrPermissions, err)
		return c
	}
	var problems []string
	var errs []error
	if perm.Withdrawals {
		problems = append(problems, "withdrawals are enabled (I-7)")
		errs = append(errs, ErrWithdrawalsEnabled)
	}
	if !perm.Reading {
		problems = append(problems, "reading is disabled")
		errs = append(errs, ErrReadingDisabled)
	}
	if perm.SpotTrading && !perm.IPRestricted {
		problems = append(problems, "spot trading is enabled without an IP restriction")
		errs = append(errs, ErrNoIPRestriction)
	}
	if len(perm.OtherEnabled) > 0 {
		problems = append(problems, "extra permissions enabled: "+strings.Join(perm.OtherEnabled, ","))
		errs = append(errs, ErrExtraPermissions)
	}
	if len(problems) > 0 {
		c.Detail = strings.Join(problems, "; ")
		c.Err = errors.Join(errs...)
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("reading on, withdrawals off, spot trading %v, IP restricted %v", perm.SpotTrading, perm.IPRestricted)
	return c
}

func checkSymbols(ctx context.Context, p Params) Check {
	c := Check{Name: "symbols_stp"}
	syms, err := p.Exchange.Symbols(ctx, p.Symbols...)
	if err != nil {
		c.Detail = "exchangeInfo: " + err.Error()
		c.Err = fmt.Errorf("%w: %v", ErrExchangeInfo, err)
		return c
	}
	byName := map[string]exchange.Symbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	var problems []string
	var errs []error
	for _, name := range p.Symbols {
		s, ok := byName[name]
		switch {
		case !ok:
			problems = append(problems, name+": not found")
			errs = append(errs, fmt.Errorf("%w: %s", ErrSymbolMissing, name))
		case !slices.Contains(s.STPAllowed, p.STPMode):
			problems = append(problems, fmt.Sprintf("%s: STP mode %s not allowed (allowed %v)", name, p.STPMode, s.STPAllowed))
			errs = append(errs, fmt.Errorf("%w: %s", ErrSTPNotAllowed, name))
		case !s.SpotEnabled:
			problems = append(problems, fmt.Sprintf("%s: spot trading not enabled (status %s)", name, s.Status))
			errs = append(errs, fmt.Errorf("%w: %s", ErrSymbolNotTrading, name))
		}
	}
	if len(problems) > 0 {
		c.Detail = strings.Join(problems, "; ")
		c.Err = errors.Join(errs...)
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%d symbols trading, STP %s allowed on all", len(p.Symbols), p.STPMode)
	return c
}

func checkChain(ctx context.Context, p Params) Check {
	c := Check{Name: "event_log"}
	if _, err := os.Stat(p.StorePath); errors.Is(err, fs.ErrNotExist) {
		// An anchor or WAL file without its database means the database was
		// deleted or the configured path is wrong: do not start on an empty log.
		for _, side := range []string{".anchor", "-wal"} {
			if _, err := os.Stat(p.StorePath + side); err == nil {
				c.Detail = fmt.Sprintf("%s exists but the database %s does not", p.StorePath+side, p.StorePath)
				c.Err = ErrDatabaseMissing
				return c
			}
		}
		c.OK = true
		c.Detail = "no database yet; it is created on first start"
		return c
	} else if err != nil {
		c.Detail = err.Error()
		c.Err = fmt.Errorf("%w: %v", ErrEventLog, err)
		return c
	}
	if err := p.VerifyChain(ctx, p.StorePath); err != nil {
		c.Detail = err.Error()
		c.Err = fmt.Errorf("%w: %v", ErrEventLog, err)
		return c
	}
	c.OK = true
	c.Detail = "hash chain verified against the anchor"
	return c
}
