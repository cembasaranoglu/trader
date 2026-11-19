// Command traderd is the trading process.
//
// It currently only loads and validates the configuration, then exits; the
// trading runtime is added wave by wave (docs/plan/01-waves.md).
// Exit codes: 0 valid, 1 invalid configuration, 2 usage error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cembasaranoglu/trader/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("traderd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the YAML configuration file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: traderd -config <file>")
		return 2
	}

	cfg, err := config.LoadFile(*path)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid:")
		printErrors(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "configuration ok: mode=%s venue=%s quote=%s\n", cfg.Mode, cfg.Venue.Primary, cfg.QuoteCurrency)
	return 0
}

// printErrors prints one line per joined error.
func printErrors(w io.Writer, err error) {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range multi.Unwrap() {
			fmt.Fprintf(w, "  - %v\n", e)
		}
		return
	}
	var fe *config.FieldError
	if errors.As(err, &fe) {
		fmt.Fprintf(w, "  - %v\n", fe)
		return
	}
	fmt.Fprintf(w, "  - %v\n", err)
}
