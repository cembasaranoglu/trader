package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("mode: paper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"valid example", []string{"-config", "../../configs/example.yaml"}, 0, "configuration ok: mode=paper venue=binance quote=USDT", ""},
		{"missing flag", nil, 2, "", "usage:"},
		{"explicit validate", []string{"validate", "-config", "../../configs/example.yaml"}, 0, "configuration ok", ""},
		{"unknown command", []string{"trade"}, 2, "", "unknown command"},
		{"observe without symbols", []string{"observe", "-config", "../../configs/example.yaml"}, 2, "", "usage:"},
		{"observe duration too long", []string{"observe", "-config", "../../configs/example.yaml", "-symbols", "BTCUSDT", "-duration", "200h"}, 2, "", "usage:"},
		{"extra argument", []string{"-config", "x.yaml", "extra"}, 2, "", "usage:"},
		{"unknown flag", []string{"-nope"}, 2, "", "flag provided but not defined"},
		{"missing file", []string{"-config", filepath.Join(dir, "none.yaml")}, 1, "", "no such file"},
		{"invalid config lists every problem", []string{"-config", bad}, 1, "", "risk: missing required field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit code: got %d, want %d (stderr: %s)", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tc.wantStdout)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
