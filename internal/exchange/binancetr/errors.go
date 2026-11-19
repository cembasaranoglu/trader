package binancetr

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for classification with errors.Is.
var (
	// ErrRateLimited is wrapped by RateLimitError (HTTP 429 or 418).
	ErrRateLimited = errors.New("binancetr: rate limited")
	// ErrBackoff is returned without contacting the server while a previous
	// 429/418 Retry-After window is still open.
	ErrBackoff = errors.New("binancetr: in rate-limit backoff")
	// ErrServer is wrapped by ServerError (HTTP 5XX). For order endpoints the
	// outcome of the request is unknown and must be reconciled by a query.
	ErrServer = errors.New("binancetr: server error, outcome unknown")
	// ErrAPI is wrapped by APIError (non-zero envelope code or 4XX).
	ErrAPI = errors.New("binancetr: api error")
	// ErrResponseTooLarge is returned when a body exceeds MaxBodyBytes.
	ErrResponseTooLarge = errors.New("binancetr: response too large")
	// ErrNoCredentials is returned when a signed endpoint is called without credentials.
	ErrNoCredentials = errors.New("binancetr: credentials required")
)

// RateLimitError reports HTTP 429 (back off) or 418 (IP banned).
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("binancetr: rate limited (HTTP %d), retry after %s", e.Status, e.RetryAfter)
}

// Unwrap returns ErrRateLimited.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// ServerError reports an HTTP 5XX response.
type ServerError struct {
	Status int
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("binancetr: HTTP %d, outcome unknown", e.Status)
}

// Unwrap returns ErrServer.
func (e *ServerError) Unwrap() error { return ErrServer }

// APIError reports a rejected request: a 4XX status or a non-zero envelope code.
type APIError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("binancetr: HTTP %d code %d: %s", e.HTTPStatus, e.Code, e.Msg)
}

// Unwrap returns ErrAPI.
func (e *APIError) Unwrap() error { return ErrAPI }
