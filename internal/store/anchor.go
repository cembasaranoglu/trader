package store

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Anchor file format: "v1 <seq> <hex hash>\n". It records a chain tip this
// host has seen; the database must still contain that event with that hash.

func writeAnchor(path string, seq int64, hash [hashLen]byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".anchor-*")
	if err != nil {
		return fmt.Errorf("store: anchor temp: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("store: anchor chmod: %w", err)
	}
	if _, err := fmt.Fprintf(f, "v1 %d %s\n", seq, hex.EncodeToString(hash[:])); err != nil {
		f.Close()
		return fmt.Errorf("store: anchor write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: anchor sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: anchor close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: anchor rename: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("store: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

func readAnchor(path string) (int64, [hashLen]byte, error) {
	var h [hashLen]byte
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, h, err
	}
	if len(b) > 256 {
		return 0, h, fmt.Errorf("%w: anchor file too large", ErrChainBroken)
	}
	parts := strings.Fields(string(b))
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, h, fmt.Errorf("%w: malformed anchor file", ErrChainBroken)
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || seq < 0 {
		return 0, h, fmt.Errorf("%w: malformed anchor seq", ErrChainBroken)
	}
	raw, err := hex.DecodeString(parts[2])
	if err != nil || len(raw) != hashLen {
		return 0, h, fmt.Errorf("%w: malformed anchor hash", ErrChainBroken)
	}
	copy(h[:], raw)
	return seq, h, nil
}
