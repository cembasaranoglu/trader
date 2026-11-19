package venuecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cembasaranoglu/trader/internal/exchange/binancetr"
)

// ErrInsecureCredentialsFile is returned when the credentials file can be
// read by users other than its owner.
var ErrInsecureCredentialsFile = errors.New("venuecheck: credentials file must not be readable by group or others (use chmod 600)")

const maxCredentialsBytes = 4 << 10

// LoadCredentials reads {"api_key": "...", "secret_key": "..."} from path.
// The file must have no group or other permissions. Error messages never
// include the file contents.
func LoadCredentials(path string) (*binancetr.Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("venuecheck: open credentials: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("venuecheck: stat credentials: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("venuecheck: credentials path is not a regular file")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, ErrInsecureCredentialsFile
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCredentialsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("venuecheck: read credentials: %w", err)
	}
	if len(data) > maxCredentialsBytes {
		return nil, errors.New("venuecheck: credentials file too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v struct {
		APIKey    string `json:"api_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := dec.Decode(&v); err != nil {
		// Do not wrap the decoder error: it can quote file contents.
		return nil, errors.New("venuecheck: credentials file is not valid JSON with only api_key and secret_key")
	}
	if v.APIKey == "" || v.SecretKey == "" {
		return nil, errors.New("venuecheck: credentials file needs both api_key and secret_key")
	}
	return &binancetr.Credentials{APIKey: v.APIKey, SecretKey: v.SecretKey}, nil
}
