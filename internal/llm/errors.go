package llm

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorKind classifies provider failures so middleware and callers can
// react without parsing provider-specific messages.
type ErrorKind string

const (
	ErrRateLimited ErrorKind = "rate_limited" // 429 — retryable, honor Retry-After
	ErrServer      ErrorKind = "server"       // 5xx — retryable
	ErrNetwork     ErrorKind = "network"      // transport failed — retryable
	ErrAuth        ErrorKind = "auth"         // 401/403 — not retryable
	ErrBadRequest  ErrorKind = "bad_request"  // 4xx — not retryable (our bug or bad config)
	ErrOverloaded  ErrorKind = "overloaded"   // provider capacity (Anthropic 529) — retryable
)

// Error is the normalized failure returned by every adapter.
type Error struct {
	Kind       ErrorKind
	StatusCode int    // HTTP status, 0 for network errors
	RetryAfter int    // seconds, from Retry-After if the provider sent one
	Provider   string // "anthropic" | "openai" | ...
	Body       string // truncated response body for diagnostics
}

func (e *Error) Error() string {
	switch {
	case e.StatusCode != 0:
		return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Provider, e.StatusCode, e.Kind, e.Body)
	default:
		return fmt.Sprintf("%s: %s: %v", e.Provider, e.Kind, e.Body)
	}
}

// Retryable reports whether the failure is worth another attempt.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrRateLimited, ErrServer, ErrNetwork, ErrOverloaded:
		return true
	default:
		return false
	}
}

// retryAfterSeconds reads the Retry-After header (delta-seconds form only;
// the HTTP-date form is rare on LLM APIs and not worth the parsing).
func retryAfterSeconds(h http.Header) int {
	if s := h.Get("Retry-After"); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// AsError extracts the normalized *Error from an error chain, if present.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// classifyStatus maps an HTTP status to an ErrorKind.
func classifyStatus(status int) ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusRequestTimeout, status == http.StatusUnprocessableEntity:
		return ErrBadRequest
	case status >= 400 && status < 500:
		return ErrBadRequest
	case status == 529: // Anthropic overloaded
		return ErrOverloaded
	case status >= 500:
		return ErrServer
	default:
		return ErrServer
	}
}
