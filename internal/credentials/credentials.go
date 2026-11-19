// Package credentials loads exchange API credentials from a private JSON
// file and turns them into signers. Secrets are never printed.
package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cembasaranoglu/trader/internal/exchange/binance"
)

// ErrInsecureCredentialsFile is returned when a credentials or key file can
// be read by users other than its owner.
var ErrInsecureCredentialsFile = errors.New("credentials: credentials and key files must not be readable by group or others (use chmod 600)")

const maxSecretFileBytes = 8 << 10

// Credentials are the raw secrets read from disk. Exactly one of SecretKey
// (HMAC) and Ed25519PEM is set. They are never printed.
type Credentials struct {
	APIKey     string
	SecretKey  string
	Ed25519PEM []byte
	// Warnings are non-secret notes about checks that could not be performed
	// on this platform (for example file ACLs on Windows).
	Warnings []string
}

// String redacts the credentials.
func (c Credentials) String() string { return "credentials.Credentials{redacted}" }

// GoString redacts the credentials.
func (c Credentials) GoString() string { return c.String() }

// Load reads a JSON credentials file:
//
//	{"api_key": "...", "secret_key": "..."}                       (HMAC key)
//	{"api_key": "...", "ed25519_private_key_file": "/path/key.pem"} (Ed25519 key)
//
// A relative ed25519_private_key_file is resolved against the directory of
// the credentials file. On Unix both files must be regular files (not
// symlinks), owned by the current user, with no group or other permissions.
// Error messages never include file contents.
func Load(path string) (*Credentials, error) {
	data, warn, err := readPrivateFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v struct {
		APIKey     string `json:"api_key"`
		SecretKey  string `json:"secret_key"`
		Ed25519Key string `json:"ed25519_private_key_file"`
	}
	if err := dec.Decode(&v); err != nil {
		// Do not wrap the decoder error: it can quote file contents.
		return nil, errors.New("credentials: credentials file is not valid JSON with api_key and secret_key or ed25519_private_key_file")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("credentials: credentials file has data after the JSON object")
	}
	if v.APIKey == "" {
		return nil, errors.New("credentials: credentials file needs api_key")
	}
	switch {
	case v.SecretKey != "" && v.Ed25519Key != "":
		return nil, errors.New("credentials: give either secret_key or ed25519_private_key_file, not both")
	case v.SecretKey != "":
		return &Credentials{APIKey: v.APIKey, SecretKey: v.SecretKey, Warnings: warn}, nil
	case v.Ed25519Key != "":
		keyPath := v.Ed25519Key
		if !filepath.IsAbs(keyPath) {
			keyPath = filepath.Join(filepath.Dir(path), keyPath)
		}
		pemBytes, keyWarn, err := readPrivateFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("credentials: ed25519 key file: %w", err)
		}
		return &Credentials{APIKey: v.APIKey, Ed25519PEM: pemBytes, Warnings: append(warn, keyWarn...)}, nil
	default:
		return nil, errors.New("credentials: credentials file needs secret_key or ed25519_private_key_file")
	}
}

// readPrivateFile reads a small regular, non-symlink file and applies the
// platform's privacy check (see checkPrivate). The file is opened and then
// compared with the Lstat result so a swap between the two calls is detected.
func readPrivateFile(path string) ([]byte, []string, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: stat: %w", err)
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, errors.New("credentials: not a regular file (symlinks are rejected)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: open: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: stat: %w", err)
	}
	if !os.SameFile(lst, st) {
		return nil, nil, errors.New("credentials: file changed while opening")
	}
	warnings, err := checkPrivate(path, st)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("credentials: read: %w", err)
	}
	if len(data) > maxSecretFileBytes {
		return nil, nil, errors.New("credentials: file too large")
	}
	return data, warnings, nil
}

// Binance returns credentials for the binance.com client: an Ed25519 signer
// when a key file was given, otherwise an HMAC signer.
func (c *Credentials) Binance() (*binance.Credentials, error) {
	var signer binance.Signer
	var err error
	switch {
	case len(c.Ed25519PEM) > 0:
		signer, err = binance.NewEd25519SignerFromPEM(c.Ed25519PEM)
	case c.SecretKey != "":
		signer, err = binance.NewHMACSigner(c.SecretKey)
	default:
		err = errors.New("credentials: no secret")
	}
	if err != nil {
		return nil, err
	}
	return &binance.Credentials{APIKey: c.APIKey, Signer: signer}, nil
}
