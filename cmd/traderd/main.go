// Command traderd is the trading process.
//
// Subcommands:
//
//	traderd [validate] -config <file>          validate the configuration (default)
//	traderd observe -config <file> -symbols A,B [-duration 24h] [-rotate-after 23h50m] [-out file]
//	                                           stream market data and report its health
//	traderd preflight -config <file> -credentials <file> -symbols A,B [-environment live|demo]
//	                                           run the start-up safety checks
//	traderd store verify -config <file> | -file <db>  verify the event log (or a backup)
//	traderd store backup -config <file>                back up the event log (read-only, safe while running)
//
// The trading runtime is added wave by wave (docs/plan/01-waves.md).
// Exit codes: 0 success, 1 failure (invalid configuration, setup error),
// 2 usage error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cembasaranoglu/trader/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage:
  traderd [validate] -config <file>
  traderd observe -config <file> -symbols A,B [-duration 24h] [-rotate-after 23h50m] [-out file]
  traderd preflight -config <file> -credentials <file> -symbols A,B [-environment live|demo]
  traderd store verify -config <file> | -file <db>
  traderd store backup -config <file>`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "validate"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "validate":
		return runValidate(args, stdout, stderr)
	case "observe":
		return runObserve(ctx, args, stdout, stderr)
	case "preflight":
		return runPreflight(ctx, args, stdout, stderr)
	case "store":
		return runStore(ctx, args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", cmd, usage)
		return 2
	}
}

// loadConfig loads and validates the configuration, printing every problem.
func loadConfig(path string, stderr io.Writer) (*config.Config, bool) {
	cfg, err := config.LoadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "configuration is invalid:")
		printErrors(stderr, err)
		return nil, false
	}
	return cfg, true
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
