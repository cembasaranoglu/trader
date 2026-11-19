// Package httpx holds transport rules shared by the exchange REST clients:
// HTTPS-only base URLs, no redirect following (so API key headers never reach
// another host) and bounded Retry-After handling.
package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Retry-After bounds. Binance documents IP bans from 2 minutes up to 3 days.
const (
	MinRetryAfter     = time.Second
	MaxRetryAfter     = 72 * time.Hour
	DefaultRetryAfter = 60 * time.Second
)

// ErrRedirect is returned when a server answers with a 3XX status. Redirects
// are never followed.
var ErrRedirect = errors.New("httpx: redirect refused")

// ParseBaseURL parses raw (or def when raw is empty) and requires an https
// URL without query, fragment or user info. Plain http is accepted only for
// loopback hosts, which tests use.
func ParseBaseURL(name, raw, def string) (*url.URL, error) {
	if raw == "" {
		raw = def
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid %s %q", name, raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !IsLoopback(u.Hostname()) {
			return nil, fmt.Errorf("%s %q: https required for non-loopback hosts", name, raw)
		}
	default:
		return nil, fmt.Errorf("%s %q: unsupported scheme", name, raw)
	}
	return u, nil
}

// IsLoopback reports whether host is localhost or a loopback IP.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NoRedirectClient returns a shallow copy of hc (or a new client with
// timeout when hc is nil) that never follows redirects. The caller's client is
// not modified.
func NoRedirectClient(hc *http.Client, timeout time.Duration) *http.Client {
	var c http.Client
	if hc != nil {
		c = *hc
	} else {
		c.Timeout = timeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// ParseRetryAfter reads a Retry-After value in whole seconds and clamps it to
// [MinRetryAfter, MaxRetryAfter]. Missing or unparsable values (including
// HTTP dates) give DefaultRetryAfter.
func ParseRetryAfter(v string) time.Duration {
	s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if errors.Is(err, strconv.ErrRange) && s > 0 {
		return MaxRetryAfter // a huge positive value: honour the longest ban
	}
	if err != nil || s <= 0 {
		return DefaultRetryAfter
	}
	if s > int64(MaxRetryAfter/time.Second) {
		return MaxRetryAfter
	}
	d := time.Duration(s) * time.Second
	if d < MinRetryAfter {
		return MinRetryAfter
	}
	return d
}

// IsNullJSON reports whether body is the JSON literal null (ignoring
// surrounding whitespace). encoding/json decodes null into a zero value
// without error, which would hide a broken response.
func IsNullJSON(body []byte) bool {
	return strings.TrimSpace(string(body)) == "null"
}
