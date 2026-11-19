package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/cembasaranoglu/trader/internal/credentials"
	"github.com/cembasaranoglu/trader/internal/exchange/binance"
	"github.com/cembasaranoglu/trader/internal/exchange/httpx"
	"github.com/cembasaranoglu/trader/internal/preflight"
)

// demoURL is Binance Spot Demo Mode's REST host (binance-spot-api-docs, demo-mode/general-info.md).
const demoURL = "https://demo-api.binance.com"

func runPreflight(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the YAML configuration file (required)")
	credPath := fs.String("credentials", "", "JSON credentials file, chmod 600 (required)")
	symbols := fs.String("symbols", "", "comma-separated symbols to check (required)")
	env := fs.String("environment", preflight.EnvLive, "live or demo")
	baseURL := fs.String("base-url", "", "override the REST signed host (testing)")
	marketURL := fs.String("market-url", "", "override the REST market-data host (testing)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	names := splitSymbols(*symbols)
	if *path == "" || *credPath == "" || len(names) == 0 || fs.NArg() != 0 ||
		(*env != preflight.EnvLive && *env != preflight.EnvDemo) {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, ok := loadConfig(*path, stderr)
	if !ok {
		return 1
	}
	creds, err := credentials.Load(*credPath)
	if err != nil {
		fmt.Fprintln(stderr, "preflight:", err)
		return 1
	}
	for _, w := range creds.Warnings {
		fmt.Fprintln(stderr, "WARNING:", w)
	}
	bc, err := creds.Binance()
	if err != nil {
		fmt.Fprintln(stderr, "preflight:", err)
		return 1
	}
	opts := binance.Options{BaseURL: *baseURL, MarketDataURL: *marketURL, Credentials: bc}
	if *env == preflight.EnvDemo {
		// The demo environment skips the /sapi permission check, so it must
		// really talk to Demo Mode: only loopback test servers may override.
		for _, u := range []*string{&opts.BaseURL, &opts.MarketDataURL} {
			if *u == "" {
				*u = demoURL
			} else if !isLoopbackURL(*u) {
				fmt.Fprintln(stderr, "preflight: -environment demo requires the Demo Mode host; URL overrides are allowed only for loopback test servers")
				return 2
			}
		}
	}
	client, err := binance.New(opts)
	if err != nil {
		fmt.Fprintln(stderr, "preflight:", err)
		return 1
	}
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rep, err := preflight.Run(rctx, preflight.Params{Exchange: client, Environment: *env, Symbols: names,
		STPMode: string(cfg.Execution.STPMode), MaxClockSkew: cfg.Ops.MaxClockSkew,
		VerifyStore: func(ctx context.Context) error { return verifyStore(ctx, cfg.Store) }})
	for _, c := range rep.Checks {
		status := "PASS"
		switch {
		case c.Skipped:
			status = "SKIP"
		case !c.OK:
			status = "FAIL"
		}
		fmt.Fprintf(stdout, "%-4s %-16s %s\n", status, c.Name, clipLine(c.Detail))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "preflight ok")
	return 0
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && httpx.IsLoopback(u.Hostname())
}

func clipLine(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
