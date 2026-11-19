// Command venuecheck measures live Binance TR facts needed by wave W1 and
// prints a shareable JSON report (no secrets, no balance amounts).
//
// Usage:
//
//	venuecheck [-symbols BTC_TRY,ETH_TRY] [-credentials ~/.config/trader/binance_tr.json] [-out report.json]
//
// Exit codes: 0 report written (it may still list errors), 1 run failed, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange/binancetr"
	"github.com/cembasaranoglu/trader/internal/venuecheck"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("venuecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	symbols := fs.String("symbols", "BTC_TRY,ETH_TRY", "comma-separated Binance TR symbols to inspect")
	credPath := fs.String("credentials", "", "optional JSON file with api_key and secret_key (chmod 600)")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	samples := fs.Int("samples", 5, "server time samples for clock skew (1..20)")
	baseURL := fs.String("base-url", binancetr.DefaultBaseURL, "Binance TR REST host")
	mainURL := fs.String("main-market-url", binancetr.DefaultMainMarketURL, "market data host for symbol type 1")
	nextURL := fs.String("next-market-url", binancetr.DefaultNextMarketURL, "market data host for other symbol types")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *samples < 1 || *samples > 20 {
		fmt.Fprintln(stderr, "usage: venuecheck [-symbols A_TRY,B_TRY] [-credentials file] [-out file] [-samples 1..20]")
		return 2
	}
	var names []string
	for _, s := range strings.Split(*symbols, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}

	opts := binancetr.Options{BaseURL: *baseURL, MainMarketURL: *mainURL, NextMarketURL: *nextURL}
	if *credPath != "" {
		creds, err := venuecheck.LoadCredentials(*credPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		opts.Credentials = creds
	}
	client, err := binancetr.New(opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rep, err := venuecheck.Run(ctx, client, venuecheck.Options{
		Symbols: names, ClockSamples: *samples, CheckAccount: opts.Credentials != nil,
		Pause: 250 * time.Millisecond,
	})
	if err != nil {
		fmt.Fprintln(stderr, "venuecheck interrupted:", err)
		return 1
	}

	w := stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(stderr, "write report:", err)
		return 1
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintf(stderr, "report written with %d error(s); see \"errors\"\n", len(rep.Errors))
	}
	return 0
}
