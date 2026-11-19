package venuecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrInsecureCredentialsFile is returned when a credentials or key file can
// be read by users other than its owner.
var ErrInsecureCredentialsFile = errors.New("venuecheck: credentials and key files must not be readable by group or others (use chmod 600)")

const maxSecretFileBytes = 8 << 10

// Credentials are the raw secrets read from disk. Exactly one of SecretKey
// (HMAC) and Ed25519PEM is set. They are never printed.
type Credentials struct {
	APIKey     string
	SecretKey  string
	Ed25519PEM []byte
}

// String redacts the credentials.
func (c Credentials) String() string { return "venuecheck.Credentials{redacted}" }

// GoString redacts the credentials.
func (c Credentials) GoString() string { return c.String() }

// LoadCredentials reads a JSON credentials file:
//
//	{"api_key": "...", "secret_key": "..."}                       (HMAC key)
//	{"api_key": "...", "ed25519_private_key_file": "/path/key.pem"} (Ed25519 key)
//
// Both the credentials file and the key file must have no group or other
// permissions. Error messages never include file contents.
func LoadCredentials(path string) (*Credentials, error) {
	data, err := readPrivateFile(path)
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
		return nil, errors.New("venuecheck: credentials file is not valid JSON with api_key and secret_key or ed25519_private_key_file")
	}
	if v.APIKey == "" {
		return nil, errors.New("venuecheck: credentials file needs api_key")
	}
	switch {
	case v.SecretKey != "" && v.Ed25519Key != "":
		return nil, errors.New("venuecheck: give either secret_key or ed25519_private_key_file, not both")
	case v.SecretKey != "":
		return &Credentials{APIKey: v.APIKey, SecretKey: v.SecretKey}, nil
	case v.Ed25519Key != "":
		pemBytes, err := readPrivateFile(v.Ed25519Key)
		if err != nil {
			return nil, fmt.Errorf("venuecheck: ed25519 key file: %w", err)
		}
		return &Credentials{APIKey: v.APIKey, Ed25519PEM: pemBytes}, nil
	default:
		return nil, errors.New("venuecheck: credentials file needs secret_key or ed25519_private_key_file")
	}
}

// readPrivateFile reads a small regular file that only its owner can access.
func readPrivateFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("venuecheck: open: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("venuecheck: stat: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("venuecheck: not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, ErrInsecureCredentialsFile
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("venuecheck: read: %w", err)
	}
	if len(data) > maxSecretFileBytes {
		return nil, errors.New("venuecheck: file too large")
	}
	return data, nil
}
