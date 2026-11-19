// Package binance is a read-side client for the Binance (binance.com) spot
// REST API. It covers server time, symbol rules, 24h tickers, market data,
// the signed account endpoint and API key permissions. It does not place orders.
//
// Public market data is read from the market-data-only host
// (data-api.binance.vision); signed endpoints and the clock used for request
// timestamps use api.binance.com.
//
// Transport rules: base URLs must be https (plain http only for loopback test
// servers), redirects are never followed, every body is size-limited, and a
// JSON null body is an error. One rate-limit backoff state is shared by both
// hosts: after a 429/418 on either host the client refuses all requests until
// Retry-After passes. This is deliberately conservative.
package binance

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cembasaranoglu/trader/internal/exchange/httpx"
)

// Default hosts and limits.
const (
	DefaultBaseURL        = "https://api.binance.com"
	DefaultMarketDataURL  = "https://data-api.binance.vision"
	DefaultRecvWindow     = 5 * time.Second
	DefaultRequestTimeout = 10 * time.Second
	// MaxBodyBytes bounds every response body (exchangeInfo for all symbols is the largest).
	MaxBodyBytes = 16 << 20
	// MaxRecvWindow is the largest recvWindow Binance accepts.
	MaxRecvWindow = 60 * time.Second
)

// Sentinel errors for classification with errors.Is.
var (
	ErrRateLimited      = errors.New("binance: rate limited")
	ErrBackoff          = errors.New("binance: in rate-limit backoff")
	ErrServer           = errors.New("binance: server error, outcome unknown")
	ErrAPI              = errors.New("binance: api error")
	ErrResponseTooLarge = errors.New("binance: response too large")
	ErrNoCredentials    = errors.New("binance: credentials required")
	ErrBadResponse      = errors.New("binance: malformed response")
	ErrClockSkew        = errors.New("binance: clock skew too large")
)

// RateLimitError reports HTTP 429 (back off) or 418 (IP banned).
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

// Error implements error.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("binance: rate limited (HTTP %d), retry after %s", e.Status, e.RetryAfter)
}

// Unwrap returns ErrRateLimited.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// ServerError reports an HTTP 5XX response. For order endpoints the outcome
// is unknown and must be reconciled by a query.
type ServerError struct{ Status int }

// Error implements error.
func (e *ServerError) Error() string {
	return fmt.Sprintf("binance: HTTP %d, outcome unknown", e.Status)
}

// Unwrap returns ErrServer.
func (e *ServerError) Unwrap() error { return ErrServer }

// APIError reports a rejected request (4XX), with the Binance error code.
type APIError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("binance: HTTP %d code %d: %s", e.HTTPStatus, e.Code, e.Msg)
}

// Unwrap returns ErrAPI.
func (e *APIError) Unwrap() error { return ErrAPI }

// Signer signs the request payload of a SIGNED endpoint.
type Signer interface {
	Sign(payload string) (string, error)
}

// HMACSigner signs with an HMAC-SHA256 secret key (hex output).
type HMACSigner struct{ secret []byte }

// NewHMACSigner returns a signer for an HMAC secret key.
func NewHMACSigner(secret string) (*HMACSigner, error) {
	if secret == "" {
		return nil, errors.New("binance: empty HMAC secret")
	}
	return &HMACSigner{secret: []byte(secret)}, nil
}

// Sign implements Signer.
func (s HMACSigner) Sign(payload string) (string, error) {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// String redacts the secret.
// It uses a value receiver so that printing a dereferenced signer is redacted too.
func (s HMACSigner) String() string { return "binance.HMACSigner{redacted}" }

// GoString redacts the secret.
func (s HMACSigner) GoString() string { return s.String() }

// Ed25519Signer signs with an Ed25519 private key (base64 output), the key
// type Binance recommends.
type Ed25519Signer struct{ key ed25519.PrivateKey }

// NewEd25519SignerFromPEM parses an unencrypted PKCS#8 "PRIVATE KEY" PEM block.
// Encrypted keys are rejected because the Go standard library cannot decrypt
// them; protect the unencrypted file with permissions instead.
func NewEd25519SignerFromPEM(data []byte) (*Ed25519Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("binance: no PEM block in Ed25519 key file")
	}
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, errors.New("binance: encrypted Ed25519 keys are not supported; generate the key without -aes256 and chmod 600 the file")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("binance: unexpected PEM block %q, want PRIVATE KEY", block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("binance: cannot parse PKCS#8 private key")
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("binance: private key is not Ed25519")
	}
	return &Ed25519Signer{key: ek}, nil
}

// Sign implements Signer.
func (s Ed25519Signer) Sign(payload string) (string, error) {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, []byte(payload))), nil
}

// String redacts the key.
func (s Ed25519Signer) String() string { return "binance.Ed25519Signer{redacted}" }

// GoString redacts the key.
func (s Ed25519Signer) GoString() string { return s.String() }

// Credentials authenticate signed endpoints.
type Credentials struct {
	APIKey string
	Signer Signer
}

// String redacts the credentials.
func (c Credentials) String() string { return "binance.Credentials{redacted}" }

// GoString redacts the credentials.
func (c Credentials) GoString() string { return c.String() }

// Options configures a Client. Zero values select documented defaults.
type Options struct {
	BaseURL       string
	MarketDataURL string
	// HTTPClient is copied; the copy never follows redirects. A nil value
	// selects a client with DefaultRequestTimeout.
	HTTPClient  *http.Client
	Credentials *Credentials
	RecvWindow  time.Duration
	// MaxClockSkew is the largest local-vs-server clock offset SyncTime
	// accepts. Zero disables the check.
	MaxClockSkew time.Duration
	Now          func() time.Time
}

// Client is safe for concurrent use.
type Client struct {
	http         *http.Client
	base         *url.URL
	market       *url.URL
	creds        *Credentials
	recvWindow   time.Duration
	maxClockSkew time.Duration
	now          func() time.Time

	mu           sync.Mutex
	blockedUntil time.Time
	offset       time.Duration // server minus local clock, set by SyncTime
	usage        map[string]string
}

// New returns a Client.
func New(opts Options) (*Client, error) {
	base, err := httpx.ParseBaseURL("BaseURL", opts.BaseURL, DefaultBaseURL)
	if err != nil {
		return nil, fmt.Errorf("binance: %w", err)
	}
	market, err := httpx.ParseBaseURL("MarketDataURL", opts.MarketDataURL, DefaultMarketDataURL)
	if err != nil {
		return nil, fmt.Errorf("binance: %w", err)
	}
	if c := opts.Credentials; c != nil && (c.APIKey == "" || c.Signer == nil) {
		return nil, errors.New("binance: credentials need an API key and a signer")
	}
	rw := opts.RecvWindow
	if rw == 0 {
		rw = DefaultRecvWindow
	}
	if rw < time.Millisecond || rw > MaxRecvWindow {
		return nil, fmt.Errorf("binance: recvWindow must be in [1ms, %s], got %s", MaxRecvWindow, rw)
	}
	if opts.MaxClockSkew < 0 {
		return nil, fmt.Errorf("binance: MaxClockSkew must not be negative, got %s", opts.MaxClockSkew)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{http: httpx.NoRedirectClient(opts.HTTPClient, DefaultRequestTimeout), base: base, market: market,
		creds: opts.Credentials, recvWindow: rw, maxClockSkew: opts.MaxClockSkew, now: now,
		usage: make(map[string]string)}, nil
}

// BlockedUntil returns the end of the current rate-limit backoff, or the zero
// time when the client is not backing off.
func (c *Client) BlockedUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockedUntil
}

// ClockOffset returns the last offset measured by SyncTime (server minus
// local). Signed requests add it to the local clock.
func (c *Client) ClockOffset() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// SyncTime measures the offset between the local clock and the signing
// host's clock, using the midpoint of the request round trip, and stores it
// for signed requests. When MaxClockSkew is set and the absolute offset
// exceeds it, the offset is not stored and an error wrapping ErrClockSkew is
// returned: the host clock must be fixed (NTP), not papered over.
func (c *Client) SyncTime(ctx context.Context) (time.Duration, error) {
	start := c.now()
	server, err := c.ServerTime(ctx)
	if err != nil {
		return 0, err
	}
	end := c.now()
	mid := start.Add(end.Sub(start) / 2)
	off := server.Sub(mid)
	abs := off
	if abs < 0 {
		abs = -abs
	}
	if c.maxClockSkew > 0 && abs > c.maxClockSkew {
		return off, fmt.Errorf("%w: offset %s exceeds %s", ErrClockSkew, off, c.maxClockSkew)
	}
	c.mu.Lock()
	c.offset = off
	c.mu.Unlock()
	return off, nil
}

// Usage returns a copy of the last rate-limit usage headers seen.
func (c *Client) Usage() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.usage))
	for k, v := range c.usage {
		out[k] = v
	}
	return out
}

type request struct {
	host   *url.URL
	path   string
	query  url.Values
	signed bool
}

// get performs a GET and decodes a 2XX JSON body into dst.
func (c *Client) get(ctx context.Context, r request, dst any) error {
	if err := c.checkBackoff(); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range r.query {
		q[k] = v
	}
	rawQuery := q.Encode()
	if r.signed {
		if c.creds == nil {
			return ErrNoCredentials
		}
		q.Set("timestamp", strconv.FormatInt(c.now().Add(c.ClockOffset()).UnixMilli(), 10))
		q.Set("recvWindow", strconv.FormatInt(c.recvWindow.Milliseconds(), 10))
		payload := q.Encode()
		sig, err := c.creds.Signer.Sign(payload)
		if err != nil {
			return fmt.Errorf("binance: sign: %w", err)
		}
		rawQuery = payload + "&signature=" + url.QueryEscape(sig)
	}
	u := *r.host
	u.Path = strings.TrimSuffix(u.Path, "/") + r.path
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("binance: build request: %w", err)
	}
	if r.signed {
		req.Header.Set("X-MBX-APIKEY", c.creds.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = r.path // drop the signed query string
		}
		return fmt.Errorf("binance: GET %s: %w", r.path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("binance: read body: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return ErrResponseTooLarge
	}
	c.recordUsage(resp.Header)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot:
		ra := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
		c.setBackoff(ra)
		return &RateLimitError{Status: resp.StatusCode, RetryAfter: ra}
	case resp.StatusCode >= 300 && resp.StatusCode <= 399:
		return fmt.Errorf("binance: GET %s: HTTP %d: %w", r.path, resp.StatusCode, httpx.ErrRedirect)
	case resp.StatusCode >= 500:
		return &ServerError{Status: resp.StatusCode}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(body, &e) != nil || e.Msg == "" {
			e.Msg = snippet(body)
		}
		return &APIError{HTTPStatus: resp.StatusCode, Code: e.Code, Msg: e.Msg}
	}
	if httpx.IsNullJSON(body) {
		return fmt.Errorf("%w: %s: null body", ErrBadResponse, r.path)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBadResponse, r.path, err)
	}
	return nil
}

func (c *Client) checkBackoff() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Before(c.blockedUntil) {
		return fmt.Errorf("%w until %s", ErrBackoff, c.blockedUntil.UTC().Format(time.RFC3339))
	}
	return nil
}

func (c *Client) setBackoff(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until := c.now().Add(d); until.After(c.blockedUntil) {
		c.blockedUntil = until
	}
}

func (c *Client) recordUsage(h http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-mbx-used-weight") || strings.HasPrefix(lk, "x-mbx-order-count") || strings.HasPrefix(lk, "x-sapi-used") {
			c.usage[lk] = strings.Join(v, ",")
		}
	}
}

func snippet(b []byte) string {
	const limit = 256
	s := strings.ToValidUTF8(strings.TrimSpace(string(b)), "?")
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…"
	}
	return s
}
