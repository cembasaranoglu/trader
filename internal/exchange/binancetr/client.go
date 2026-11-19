// Package binancetr is a read-side client for the Binance TR Open API
// (https://www.binance.tr/apidocs). It covers server time, symbol rules,
// market data and the signed account endpoint. It does not place orders.
//
// Binance TR's API differs from the global Binance API: REST paths live under
// /open/v1, responses use a {code,msg,data,timestamp} envelope, symbols use an
// underscore (BTC_TRY), and market data is served from per-symbol-type hosts.
package binancetr

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default hosts as documented by Binance TR.
const (
	DefaultBaseURL        = "https://www.binance.tr"
	DefaultMainMarketURL  = "https://api.binance.me"       // symbol type 1
	DefaultNextMarketURL  = "https://cloudme-tr.2meta.app" // other symbol types
	DefaultRecvWindow     = 5 * time.Second
	DefaultRequestTimeout = 10 * time.Second
	// MaxBodyBytes bounds every response body read by the client.
	MaxBodyBytes = 8 << 20
	// defaultRetryAfter is used when a 429/418 carries no usable Retry-After header.
	defaultRetryAfter = 60 * time.Second
)

// Credentials authenticate signed endpoints. The secret is never printed.
type Credentials struct {
	APIKey    string
	SecretKey string
}

// String redacts the credentials so they cannot leak through %v or logs.
func (c Credentials) String() string { return "binancetr.Credentials{redacted}" }

// GoString redacts the credentials for %#v.
func (c Credentials) GoString() string { return c.String() }

// Options configures a Client. Zero values select documented defaults.
type Options struct {
	BaseURL       string
	MainMarketURL string
	NextMarketURL string
	HTTPClient    *http.Client // nil: a client with DefaultRequestTimeout
	Credentials   *Credentials // nil: signed endpoints return ErrNoCredentials
	RecvWindow    time.Duration
	Now           func() time.Time // nil: time.Now; injectable for tests
}

// Client is safe for concurrent use.
type Client struct {
	http       *http.Client
	base       *url.URL
	mainMarket *url.URL
	nextMarket *url.URL
	creds      *Credentials
	recvWindow time.Duration
	now        func() time.Time

	mu           sync.Mutex
	blockedUntil time.Time
	usage        map[string]string // last seen X-MBX-USED-WEIGHT-* / X-MBX-ORDER-COUNT-* headers
}

// New returns a Client. It fails on malformed URLs or invalid credentials.
func New(opts Options) (*Client, error) {
	pick := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	parse := func(name, raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("binancetr: invalid %s %q", name, raw)
		}
		return u, nil
	}
	base, err := parse("BaseURL", pick(opts.BaseURL, DefaultBaseURL))
	if err != nil {
		return nil, err
	}
	mainM, err := parse("MainMarketURL", pick(opts.MainMarketURL, DefaultMainMarketURL))
	if err != nil {
		return nil, err
	}
	nextM, err := parse("NextMarketURL", pick(opts.NextMarketURL, DefaultNextMarketURL))
	if err != nil {
		return nil, err
	}
	if c := opts.Credentials; c != nil && (c.APIKey == "" || c.SecretKey == "") {
		return nil, errors.New("binancetr: credentials must include both API key and secret key")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultRequestTimeout}
	}
	rw := opts.RecvWindow
	if rw == 0 {
		rw = DefaultRecvWindow
	}
	if rw < 0 || rw > 60*time.Second {
		return nil, fmt.Errorf("binancetr: recvWindow must be in (0, 60s], got %s", rw)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		http: hc, base: base, mainMarket: mainM, nextMarket: nextM,
		creds: opts.Credentials, recvWindow: rw, now: now,
		usage: make(map[string]string),
	}, nil
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

// BlockedUntil returns the end of the current rate-limit backoff, if any.
func (c *Client) BlockedUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockedUntil
}

// request describes one HTTP call.
type request struct {
	method string
	host   *url.URL
	path   string
	query  url.Values
	signed bool
}

// do performs the request and returns the raw body of a 2XX response.
func (c *Client) do(ctx context.Context, r request) ([]byte, error) {
	if err := c.checkBackoff(); err != nil {
		return nil, err
	}
	q := r.query
	if q == nil {
		q = url.Values{}
	}
	var rawQuery string
	if r.signed {
		if c.creds == nil {
			return nil, ErrNoCredentials
		}
		q.Set("timestamp", strconv.FormatInt(c.now().UnixMilli(), 10))
		q.Set("recvWindow", strconv.FormatInt(c.recvWindow.Milliseconds(), 10))
		payload := q.Encode()
		rawQuery = payload + "&signature=" + sign(c.creds.SecretKey, payload)
	} else {
		rawQuery = q.Encode()
	}

	u := *r.host
	u.Path = strings.TrimSuffix(u.Path, "/") + r.path
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("binancetr: build request: %w", err)
	}
	if r.signed {
		req.Header.Set("X-MBX-APIKEY", c.creds.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Drop the query string (timestamp, signature) from transport errors.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = r.path
		}
		return nil, fmt.Errorf("binancetr: %s %s: %w", r.method, r.path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("binancetr: read body: %w", err)
	}
	if len(body) > MaxBodyBytes {
		return nil, ErrResponseTooLarge
	}
	c.recordUsage(resp.Header)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot:
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		c.setBackoff(ra)
		return nil, &RateLimitError{Status: resp.StatusCode, RetryAfter: ra}
	case resp.StatusCode >= 500:
		return nil, &ServerError{Status: resp.StatusCode}
	case resp.StatusCode >= 400:
		code, msg := envelopeError(body)
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: code, Msg: msg}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &APIError{HTTPStatus: resp.StatusCode, Msg: "unexpected status"}
	}
	return body, nil
}

func (c *Client) checkBackoff() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); now.Before(c.blockedUntil) {
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
		if strings.HasPrefix(lk, "x-mbx-used-weight") || strings.HasPrefix(lk, "x-mbx-order-count") {
			c.usage[lk] = strings.Join(v, ",")
		}
	}
}

// sign returns the hex HMAC-SHA256 of payload keyed by secret.
func sign(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// parseRetryAfter reads a Retry-After value in seconds, falling back to a
// conservative default when the header is missing or malformed.
func parseRetryAfter(v string) time.Duration {
	s, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || s <= 0 {
		return defaultRetryAfter
	}
	return time.Duration(s) * time.Second
}

// envelope is the documented Binance TR response wrapper. Some order
// endpoints use "message" in place of "msg".
type envelope struct {
	Code      *int            `json:"code"`
	Msg       string          `json:"msg"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Timestamp int64           `json:"timestamp"`
}

// Shape names the response format a call returned.
type Shape string

// Response shapes. The documentation shows envelopes for every endpoint, but
// the market-data hosts mirror Binance global paths, so a raw body is accepted too.
const (
	ShapeEnvelope Shape = "envelope"
	ShapeRaw      Shape = "raw"
)

// unwrap returns the payload of an enveloped body, or the body itself when it
// is not enveloped. A non-zero envelope code becomes an *APIError.
func unwrap(body []byte) (json.RawMessage, envelope, Shape, error) {
	var env envelope
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &env); err != nil {
			return nil, env, "", fmt.Errorf("binancetr: decode response: %w", err)
		}
		if env.Code != nil {
			if *env.Code != 0 {
				return nil, env, ShapeEnvelope, &APIError{HTTPStatus: http.StatusOK, Code: *env.Code, Msg: env.message()}
			}
			return env.Data, env, ShapeEnvelope, nil
		}
	}
	return json.RawMessage(trimmed), env, ShapeRaw, nil
}

func (e envelope) message() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Message
}

// envelopeError extracts code and message from an error body, if present.
func envelopeError(body []byte) (int, string) {
	var env envelope
	if err := json.Unmarshal(body, &env); err == nil && env.Code != nil {
		return *env.Code, env.message()
	}
	const max = 256
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return 0, s
}
