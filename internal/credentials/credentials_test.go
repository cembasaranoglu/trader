package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows; see private_other.go")
	}
	dir := t.TempDir()
	write := func(name, body string, perm os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil { // WriteFile honors umask
			t.Fatal(err)
		}
		return p
	}
	hmac := write("hmac.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o600)
	c, err := Load(hmac)
	if err != nil || c.SecretKey != "s3cr3t" || c.Ed25519PEM != nil {
		t.Fatalf("hmac: %v", err)
	}
	pemPath := write("key.pem", "-----BEGIN PRIVATE KEY-----\ns3cr3t\n-----END PRIVATE KEY-----\n", 0o600)
	ed := write("ed.json", `{"api_key":"k","ed25519_private_key_file":"`+pemPath+`"}`, 0o600)
	c, err = Load(ed)
	if err != nil || c.SecretKey != "" || !strings.Contains(string(c.Ed25519PEM), "PRIVATE KEY") {
		t.Fatalf("ed25519: %v", err)
	}
	if s := strings.Join([]string{c.String(), c.GoString()}, " "); strings.Contains(s, "s3cr3t") {
		t.Fatal("credentials printed")
	}
	rel := write("rel.json", `{"api_key":"k","ed25519_private_key_file":"key.pem"}`, 0o600)
	if c, err = Load(rel); err != nil || c.Ed25519PEM == nil {
		t.Fatalf("relative key path must resolve against the credentials directory: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(hmac, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("symlinked credentials file must be rejected")
	}

	openPEM := write("open.pem", "x", 0o644)
	tests := []struct {
		name string
		path string
		want error
	}{
		{"group readable", write("open.json", `{"api_key":"k","secret_key":"s3cr3t"}`, 0o640), ErrInsecureCredentialsFile},
		{"key file group readable", write("edopen.json", `{"api_key":"k","ed25519_private_key_file":"`+openPEM+`"}`, 0o600), ErrInsecureCredentialsFile},
		{"both secrets", write("both.json", `{"api_key":"k","secret_key":"s3cr3t","ed25519_private_key_file":"x"}`, 0o600), nil},
		{"no secret", write("half.json", `{"api_key":"k"}`, 0o600), nil},
		{"no api key", write("nokey.json", `{"secret_key":"s3cr3t"}`, 0o600), nil},
		{"unknown field", write("extra.json", `{"api_key":"k","secret_key":"s3cr3t","x":1}`, 0o600), nil},
		{"not json", write("bad.json", `api_key=s3cr3t`, 0o600), nil},
		{"trailing data", write("trail.json", `{"api_key":"k","secret_key":"s3cr3t"} {"x":"s3cr3t"}`, 0o600), nil},
		{"directory", dir, nil},
		{"missing file", filepath.Join(dir, "none.json"), os.ErrNotExist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(tc.path)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error leaks file contents: %v", err)
			}
		})
	}
}
