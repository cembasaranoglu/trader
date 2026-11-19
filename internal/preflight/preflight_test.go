package preflight

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
)

type fakeEx struct {
	offset   time.Duration
	syncErr  error
	rtt      time.Duration
	clock    *fakeClock
	perm     exchange.KeyPermissions
	permErr  error
	permCall bool
	syms     []exchange.Symbol
	symErr   error
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (f *fakeEx) SyncTime(context.Context) (time.Duration, error) {
	f.clock.Advance(f.rtt)
	return f.offset, f.syncErr
}
func (f *fakeEx) KeyPermissions(context.Context) (exchange.KeyPermissions, error) {
	f.permCall = true
	return f.perm, f.permErr
}
func (f *fakeEx) Symbols(_ context.Context, names ...string) ([]exchange.Symbol, error) {
	return f.syms, f.symErr
}

func goodEx() *fakeEx {
	return &fakeEx{
		offset: 50 * time.Millisecond, rtt: 100 * time.Millisecond,
		clock: &fakeClock{t: time.Unix(1700000000, 0)},
		perm:  exchange.KeyPermissions{Reading: true, SpotTrading: true, IPRestricted: true},
		syms: []exchange.Symbol{
			{Name: "BTCUSDT", SpotEnabled: true, Status: "TRADING", STPAllowed: []string{"EXPIRE_TAKER", "EXPIRE_MAKER"}},
			{Name: "ETHUSDT", SpotEnabled: true, Status: "TRADING", STPAllowed: []string{"EXPIRE_TAKER"}},
		},
	}
}

var errChainBroken = errors.New("hash chain broken")

func params(ex *fakeEx, dir string) Params {
	return Params{Exchange: ex, Environment: EnvLive, Symbols: []string{"BTCUSDT", "ETHUSDT"}, STPMode: "EXPIRE_TAKER",
		MaxClockSkew: 500 * time.Millisecond,
		VerifyStore:  func(context.Context) error { return nil }, Now: ex.clock.Now}
}

func failed(rep Report) []string {
	var out []string
	for _, c := range rep.Checks {
		if !c.OK && !c.Skipped {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestAllChecksPass(t *testing.T) {
	t.Parallel()
	rep, err := Run(context.Background(), params(goodEx(), t.TempDir()))
	if err != nil || !rep.OK || len(rep.Checks) != 4 {
		t.Fatalf("%v %+v", err, rep)
	}
	if rep.Checks[3].Name != "event_log" || !strings.Contains(rep.Checks[3].Detail, "hash chain verified") {
		t.Fatalf("event log check: %+v", rep.Checks[3])
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mut  func(*fakeEx, *Params)
		fail string
		want error
	}{
		"withdrawals enabled":       {func(e *fakeEx, _ *Params) { e.perm.Withdrawals = true }, "key_permissions", ErrWithdrawalsEnabled},
		"extra permission":          {func(e *fakeEx, _ *Params) { e.perm.OtherEnabled = []string{"enableMargin"} }, "key_permissions", ErrExtraPermissions},
		"unknown true permission":   {func(e *fakeEx, _ *Params) { e.perm.OtherEnabled = []string{"newShinyPermission"} }, "key_permissions", ErrExtraPermissions},
		"trading without IP limit":  {func(e *fakeEx, _ *Params) { e.perm.IPRestricted = false }, "key_permissions", ErrNoIPRestriction},
		"reading disabled":          {func(e *fakeEx, _ *Params) { e.perm.Reading = false }, "key_permissions", ErrReadingDisabled},
		"permission call fails":     {func(e *fakeEx, _ *Params) { e.permErr = errors.New("malformed response") }, "key_permissions", ErrPermissions},
		"clock offset too large":    {func(e *fakeEx, _ *Params) { e.offset = 600 * time.Millisecond }, "clock", ErrClockSkew},
		"negative offset too large": {func(e *fakeEx, _ *Params) { e.offset = -600 * time.Millisecond }, "clock", ErrClockSkew},
		"slow round trip":           {func(e *fakeEx, _ *Params) { e.offset = 300 * time.Millisecond; e.rtt = 500 * time.Millisecond }, "clock", ErrClockSkew},
		"server time fails":         {func(e *fakeEx, _ *Params) { e.syncErr = errors.New("clock skew too large") }, "clock", ErrServerTime},
		"stp not allowed":           {func(e *fakeEx, p *Params) { p.STPMode = "EXPIRE_MAKER" }, "symbols_stp", ErrSTPNotAllowed},
		"symbol missing":            {func(e *fakeEx, p *Params) { p.Symbols = append(p.Symbols, "SOLUSDT") }, "symbols_stp", ErrSymbolMissing},
		"empty STP list":            {func(e *fakeEx, _ *Params) { e.syms[0].STPAllowed = nil }, "symbols_stp", ErrSTPNotAllowed},
		"symbol halted":             {func(e *fakeEx, _ *Params) { e.syms[1].SpotEnabled = false; e.syms[1].Status = "HALT" }, "symbols_stp", ErrSymbolNotTrading},
		"exchangeInfo fails":        {func(e *fakeEx, _ *Params) { e.symErr = errors.New("418") }, "symbols_stp", ErrExchangeInfo},
		"chain broken": {func(e *fakeEx, p *Params) {
			p.VerifyStore = func(context.Context) error { return errChainBroken }
		}, "event_log", ErrEventLog},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ex := goodEx()
			p := params(ex, t.TempDir())
			tc.mut(ex, &p)
			rep, err := Run(context.Background(), p)
			if !errors.Is(err, ErrRefused) || rep.OK {
				t.Fatalf("want refusal, got %v", err)
			}
			if f := failed(rep); len(f) != 1 || f[0] != tc.fail {
				t.Fatalf("failed checks %v, want [%s]", f, tc.fail)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v does not wrap %v", err, tc.want)
			}
			if len(rep.Checks) != 4 {
				t.Fatal("every check must run and be reported")
			}
		})
	}
}

func TestClockBoundaryIsInclusive(t *testing.T) {
	t.Parallel()
	ex := goodEx()
	ex.offset, ex.rtt = 400*time.Millisecond, 200*time.Millisecond // 400 + 100 = 500
	if _, err := Run(context.Background(), params(ex, t.TempDir())); err != nil {
		t.Fatalf("exactly at the limit must pass: %v", err)
	}
}

func TestDemoSkipsPermissionsOnlyByConfiguration(t *testing.T) {
	t.Parallel()
	ex := goodEx()
	ex.permErr = errors.New("404")
	p := params(ex, t.TempDir())
	p.Environment = EnvDemo
	rep, err := Run(context.Background(), p)
	if err != nil || ex.permCall || !rep.Checks[1].Skipped {
		t.Fatalf("demo must skip the permission call: %v %+v", err, rep.Checks[1])
	}
	// In the live environment a failing call is a failure, never a skip.
	ex2 := goodEx()
	ex2.permErr = errors.New("404")
	if _, err := Run(context.Background(), params(ex2, t.TempDir())); !errors.Is(err, ErrRefused) {
		t.Fatal("live: failing permission call must refuse")
	}
}

func TestInvalidParams(t *testing.T) {
	t.Parallel()
	for name, mut := range map[string]func(*Params){
		"no exchange":   func(p *Params) { p.Exchange = nil },
		"bad env":       func(p *Params) { p.Environment = "staging" },
		"no symbols":    func(p *Params) { p.Symbols = nil },
		"no skew limit": func(p *Params) { p.MaxClockSkew = 0 },
		"no verifier":   func(p *Params) { p.VerifyStore = nil },
	} {
		p := params(goodEx(), t.TempDir())
		mut(&p)
		if _, err := Run(context.Background(), p); err == nil || errors.Is(err, ErrRefused) {
			t.Errorf("%s: want parameter error, got %v", name, err)
		}
	}
}
