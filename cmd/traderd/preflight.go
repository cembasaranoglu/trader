package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/cembasaranoglu/trader/internal/credentials"
	"github.com/cembasaranoglu/trader/internal/exchange/binance"
	"github.com/cembasaranoglu/trader/internal/preflight"
	"github.com/cembasaranoglu/trader/internal/store"
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
		if opts.BaseURL == "" {
			opts.BaseURL = demoURL
		}
		if opts.MarketDataURL == "" {
			opts.MarketDataURL = demoURL
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
		STPMode: string(cfg.Execution.STPMode), MaxClockSkew: cfg.Ops.MaxClockSkew, StorePath: cfg.Store.Path,
		VerifyChain: store.VerifyFile})
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

func clipLine(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func runStore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "verify" && args[0] != "backup") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	sub := args[0]
	flags := flag.NewFlagSet("store "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to the YAML configuration file (required)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *path == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, ok := loadConfig(*path, stderr)
	if !ok {
		return 1
	}
	if _, err := os.Stat(cfg.Store.Path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "store: no database at %s\n", cfg.Store.Path)
		return 1
	}
	switch sub {
	case "verify":
		if err := store.VerifyFile(ctx, cfg.Store.Path); err != nil {
			fmt.Fprintln(stderr, "store verify:", err)
			return 1
		}
		fmt.Fprintf(stdout, "store ok: %s\n", cfg.Store.Path)
		return 0
	default:
		s, err := store.Open(ctx, cfg.Store.Path, store.Options{})
		if err != nil {
			fmt.Fprintln(stderr, "store backup:", err)
			return 1
		}
		dst := filepath.Join(cfg.Store.BackupDir, "trader-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
		sum, berr := s.Backup(ctx, dst)
		cerr := s.Close()
		if berr != nil {
			fmt.Fprintln(stderr, "store backup:", berr)
			return 1
		}
		if cerr != nil {
			fmt.Fprintln(stderr, "store backup: close:", cerr)
			return 1
		}
		fmt.Fprintf(stdout, "backup ok: %s sha256 %s\n", dst, sum)
		return 0
	}
}
