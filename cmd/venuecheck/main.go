// Command venuecheck measures live exchange facts and prints a shareable JSON
// report (no secrets, no balance amounts).
//
// Usage:
//
//	venuecheck [-venue binance|binance_tr] [-symbols A,B] [-credentials file] [-out file]
//
// Exit codes: 0 report written (it may still list errors or warnings),
// 1 run failed, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange"
	"github.com/cembasaranoglu/trader/internal/exchange/binance"
	"github.com/cembasaranoglu/trader/internal/exchange/binancetr"
	"github.com/cembasaranoglu/trader/internal/venuecheck"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

type venueDefaults struct {
	quote   string
	symbols string
}

var defaults = map[string]venueDefaults{
	"binance":    {quote: "USDT", symbols: "BTCUSDT,ETHUSDT"},
	"binance_tr": {quote: "TRY", symbols: "BTC_TRY,ETH_TRY"},
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("venuecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	venueName := fs.String("venue", "binance", "binance (binance.com) or binance_tr")
	symbols := fs.String("symbols", "", "comma-separated symbols to inspect (default per venue)")
	credPath := fs.String("credentials", "", "optional JSON credentials file (chmod 600)")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	samples := fs.Int("samples", 5, "server time samples for clock skew (1..20)")
	topN := fs.Int("top", 20, "liquidity ranking size, 0..100 (binance only)")
	baseURL := fs.String("base-url", "", "override the REST/signed host (testing)")
	marketURL := fs.String("market-url", "", "override the market data host (testing)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d, ok := defaults[*venueName]
	if !ok || fs.NArg() != 0 || *samples < 1 || *samples > 20 || *topN < 0 || *topN > 100 {
		fmt.Fprintln(stderr, "usage: venuecheck [-venue binance|binance_tr] [-symbols A,B] [-credentials file] [-out file] [-samples 1..20] [-top 0..100]")
		return 2
	}
	if *symbols == "" {
		*symbols = d.symbols
	}
	var names []string
	for _, s := range strings.Split(*symbols, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}

	var creds *venuecheck.Credentials
	if *credPath != "" {
		c, err := venuecheck.LoadCredentials(*credPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		creds = c
	}
	v, err := newVenue(*venueName, *baseURL, *marketURL, creds)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rep, err := venuecheck.Run(ctx, v, venuecheck.Options{
		Venue: *venueName, Quote: d.quote, Symbols: names, TopN: *topN,
		ClockSamples: *samples, CheckAccount: creds != nil, Pause: 250 * time.Millisecond,
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
	for _, warn := range rep.Warnings {
		fmt.Fprintln(stderr, "WARNING:", warn)
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintf(stderr, "report written with %d error(s); see \"errors\"\n", len(rep.Errors))
	}
	return 0
}

func newVenue(name, baseURL, marketURL string, creds *venuecheck.Credentials) (venuecheck.Venue, error) {
	switch name {
	case "binance":
		opts := binance.Options{BaseURL: baseURL, MarketDataURL: marketURL}
		if creds != nil {
			var signer binance.Signer
			var err error
			if creds.SecretKey != "" {
				signer, err = binance.NewHMACSigner(creds.SecretKey)
			} else {
				signer, err = binance.NewEd25519SignerFromPEM(creds.Ed25519PEM)
			}
			if err != nil {
				return nil, err
			}
			opts.Credentials = &binance.Credentials{APIKey: creds.APIKey, Signer: signer}
		}
		c, err := binance.New(opts)
		if err != nil {
			return nil, err
		}
		return binanceVenue{c}, nil
	case "binance_tr":
		opts := binancetr.Options{BaseURL: baseURL, MainMarketURL: marketURL, NextMarketURL: marketURL}
		if creds != nil {
			if creds.SecretKey == "" {
				return nil, errors.New("venuecheck: binance_tr supports only HMAC keys (secret_key)")
			}
			opts.Credentials = &binancetr.Credentials{APIKey: creds.APIKey, SecretKey: creds.SecretKey}
		}
		c, err := binancetr.New(opts)
		if err != nil {
			return nil, err
		}
		return binanceTRVenue{c}, nil
	}
	return nil, fmt.Errorf("venuecheck: unknown venue %q", name)
}

// binanceVenue adapts *binance.Client to venuecheck.Venue: it requests all
// symbols and keeps the optional ticker and permission methods.
type binanceVenue struct{ *binance.Client }

func (b binanceVenue) Symbols(ctx context.Context) ([]exchange.Symbol, error) {
	return b.Client.Symbols(ctx)
}

// binanceTRVenue adapts *binancetr.Client, dropping the response-shape result.
type binanceTRVenue struct{ *binancetr.Client }

func (b binanceTRVenue) Depth(ctx context.Context, s exchange.Symbol, limit int) (exchange.OrderBook, error) {
	ob, _, err := b.Client.Depth(ctx, s, limit)
	return ob, err
}

func (b binanceTRVenue) Klines(ctx context.Context, s exchange.Symbol, interval string, limit int) ([]exchange.Kline, error) {
	ks, _, err := b.Client.Klines(ctx, s, interval, limit)
	return ks, err
}
