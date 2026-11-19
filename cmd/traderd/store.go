package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cembasaranoglu/trader/internal/config"
	"github.com/cembasaranoglu/trader/internal/credentials"
	"github.com/cembasaranoglu/trader/internal/store"
)

// Environment variables that may carry a connection string for a local
// database only (Docker Compose, loopback); production reads files.
const (
	envStoreDSN        = "TRADER_STORE_DSN"
	envStoreMigrateDSN = "TRADER_STORE_MIGRATE_DSN"
)

// resolveDSN returns the connection string from the environment variable
// (local databases only) or from the private file.
func resolveDSN(envName, file string, stderr io.Writer) (string, error) {
	if v := os.Getenv(envName); v != "" {
		cfg, err := store.ParseDSN(v, 0, 0)
		if err != nil {
			return "", fmt.Errorf("%s: %w", envName, err)
		}
		if !store.AllHostsLocal(cfg) {
			return "", fmt.Errorf("%s may name only local databases (Docker service, loopback, socket); put a remote connection string in the configured file", envName)
		}
		return v, nil
	}
	if file == "" {
		return "", fmt.Errorf("no connection string: set %s for a local database or configure the file", envName)
	}
	v, warns, err := credentials.ReadSecret(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", file, err)
	}
	for _, w := range warns {
		fmt.Fprintln(stderr, "WARNING:", w)
	}
	return v, nil
}

// verifyStore checks, read-only, that store.dsn_file names the application
// role (not the owner or a role with more privileges) and that the schema,
// guards, chain and anchor verify.
func verifyStore(ctx context.Context, sc config.Store) error {
	dsn, err := resolveDSN(envStoreDSN, sc.DSNFile, io.Discard)
	if err != nil {
		return err
	}
	s, err := store.OpenUnchecked(ctx, dsn, store.Options{AnchorPath: sc.AnchorPath})
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Verify(ctx); err != nil {
		return err
	}
	return s.CheckPrivileges(ctx)
}

const storeUsage = `usage:
  traderd store migrate -config <file>        apply migrations as the owner role, grant the application role
  traderd store verify -config <file>         verify schema, guards, chain and anchor (read-only)
  traderd store verify -export <file>         verify an export offline (its .sha256 first, when present)
  traderd store export -config <file>         write a verifiable export to store.export_dir
  traderd store anchor-init -config <file>    create the anchor on a host that has none`

func runStore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, storeUsage)
		return 2
	}
	sub := args[0]
	switch sub {
	case "migrate", "verify", "export", "anchor-init":
	default:
		fmt.Fprintln(stderr, storeUsage)
		return 2
	}
	flags := flag.NewFlagSet("store "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to the YAML configuration file")
	export := flags.String("export", "", "verify: an export file instead of the database")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || (*export != "" && (sub != "verify" || *path != "")) || (*export == "" && *path == "") {
		fmt.Fprintln(stderr, storeUsage)
		return 2
	}
	if *export != "" {
		return verifyExportFile(*export, stdout, stderr)
	}
	cfg, ok := loadConfig(*path, stderr)
	if !ok {
		return 1
	}
	sc := cfg.Store
	rctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	switch sub {
	case "migrate":
		dsn, err := resolveDSN(envStoreMigrateDSN, sc.MigrateDSNFile, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "store migrate:", err)
			return 1
		}
		applied, err := store.Migrate(rctx, dsn, sc.AppRole)
		if err != nil {
			fmt.Fprintln(stderr, "store migrate:", err)
			return 1
		}
		fmt.Fprintf(stdout, "migrate ok: applied %v, privileges granted to %s\n", applied, sc.AppRole)
		return 0
	case "verify":
		if err := verifyStore(rctx, sc); err != nil {
			fmt.Fprintln(stderr, "store verify:", err)
			return 1
		}
		fmt.Fprintln(stdout, "store ok")
		return 0
	}
	dsn, err := resolveDSN(envStoreDSN, sc.DSNFile, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "store %s: %v\n", sub, err)
		return 1
	}
	s, err := store.OpenUnchecked(rctx, dsn, store.Options{AnchorPath: sc.AnchorPath})
	if err != nil {
		fmt.Fprintf(stderr, "store %s: %v\n", sub, err)
		return 1
	}
	defer s.Close()
	if sub == "anchor-init" {
		n, err := s.InitAnchor(rctx)
		if err != nil {
			fmt.Fprintln(stderr, "store anchor-init:", err)
			return 1
		}
		fmt.Fprintf(stdout, "anchor ok: %s at seq %d\n", sc.AnchorPath, n)
		return 0
	}
	dst, n, sum, err := writeExport(rctx, s, sc.ExportDir)
	if err != nil {
		fmt.Fprintln(stderr, "store export:", err)
		return 1
	}
	fmt.Fprintf(stdout, "export ok: %s events=%d sha256 %s\n", dst, n, sum)
	return 0
}

// writeExport writes export_dir/trader-events-<UTC>.jsonl (0600, created
// exclusively, synced) and a .sha256 next to it. A failed export leaves no
// file behind.
func writeExport(ctx context.Context, s *store.Store, dir string) (string, int64, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, "", err
	}
	dst := filepath.Join(dir, "trader-events-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".jsonl")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, "", err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(dst)
			os.Remove(dst + ".sha256")
		}
	}()
	h := sha256.New()
	n, _, err := s.Export(ctx, io.MultiWriter(f, h))
	if err != nil {
		return "", 0, "", err
	}
	if err := f.Sync(); err != nil {
		return "", 0, "", err
	}
	if err := f.Close(); err != nil {
		return "", 0, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.WriteFile(dst+".sha256", []byte(sum+"  "+filepath.Base(dst)+"\n"), 0o600); err != nil {
		return "", 0, "", err
	}
	if err := syncDir(dir); err != nil {
		return "", 0, "", err
	}
	ok = true
	return dst, n, sum, nil
}

// verifyExportFile checks the .sha256 sidecar (when present) and then the
// chain inside the export.
func verifyExportFile(path string, stdout, stderr io.Writer) int {
	if b, err := os.ReadFile(path + ".sha256"); err == nil {
		want := strings.Fields(string(b))
		got, err := fileSHA256(path)
		if err != nil || len(want) == 0 || want[0] != got {
			fmt.Fprintf(stderr, "store verify: %s does not match its .sha256\n", path)
			return 1
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "store verify:", err)
		return 1
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "store verify:", err)
		return 1
	}
	defer f.Close()
	n, tip, err := store.VerifyExport(f)
	if err != nil {
		fmt.Fprintln(stderr, "store verify:", err)
		return 1
	}
	fmt.Fprintf(stdout, "export ok: %s events=%d tip %s\n", path, n, hex.EncodeToString(tip[:]))
	return 0
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
