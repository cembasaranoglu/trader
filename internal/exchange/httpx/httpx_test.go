package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw string
		ok  bool
	}{
		{"https://api.binance.com", true},
		{"https://api.binance.com/", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://localhost:1", true},
		{"http://api.binance.com", false},
		{"ftp://api.binance.com", false},
		{"https://user:pw@api.binance.com", false},
		{"https://api.binance.com?x=1", false},
		{"https://api.binance.com#f", false},
		{"https://", false},
		{"://bad", false},
	}
	for _, tc := range tests {
		_, err := ParseBaseURL("BaseURL", tc.raw, "")
		if (err == nil) != tc.ok {
			t.Errorf("%q: ok=%v, err=%v", tc.raw, tc.ok, err)
		}
	}
	u, err := ParseBaseURL("BaseURL", "", "https://example.com")
	if err != nil || u.Host != "example.com" {
		t.Fatalf("default not used: %v %v", u, err)
	}
}

func TestNoRedirectClient(t *testing.T) {
	t.Parallel()
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	orig := &http.Client{Timeout: 3 * time.Second}
	c := NoRedirectClient(orig, 0)
	if orig.CheckRedirect != nil {
		t.Fatal("caller's client was modified")
	}
	if c.Timeout != 3*time.Second {
		t.Fatalf("timeout not copied: %s", c.Timeout)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits != 0 {
		t.Fatalf("redirect followed: status=%d hits=%d", resp.StatusCode, hits)
	}
	if NoRedirectClient(nil, 7*time.Second).Timeout != 7*time.Second {
		t.Fatal("nil client must get the timeout")
	}
	if !errors.Is(ErrRedirect, ErrRedirect) {
		t.Fatal("sentinel")
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	tests := map[string]time.Duration{
		"":                              DefaultRetryAfter,
		"abc":                           DefaultRetryAfter,
		"-5":                            DefaultRetryAfter,
		"0":                             DefaultRetryAfter,
		"Wed, 21 Oct 2015 07:28:00 GMT": DefaultRetryAfter,
		"1":                             time.Second,
		" 120 ":                         120 * time.Second,
		"259200":                        MaxRetryAfter,
		"9999999999999999":              MaxRetryAfter,
		"99999999999999999999999":       MaxRetryAfter, // overflows int64
	}
	for in, want := range tests {
		if got := ParseRetryAfter(in); got != want {
			t.Errorf("ParseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestIsNullJSON(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"null": true, " null\n": true, "{}": false, "[]": false, "nullx": false} {
		if IsNullJSON([]byte(in)) != want {
			t.Errorf("IsNullJSON(%q) != %v", in, want)
		}
	}
}
