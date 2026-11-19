package main

import (
	"flag"
	"fmt"
	"io"
)

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the YAML configuration file (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, ok := loadConfig(*path, stderr)
	if !ok {
		return 1
	}
	fmt.Fprintf(stdout, "configuration ok: mode=%s venue=%s quote=%s\n", cfg.Mode, cfg.Venue.Primary, cfg.QuoteCurrency)
	return 0
}
