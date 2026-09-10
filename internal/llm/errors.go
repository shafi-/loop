package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

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

// ErrorKind classifies provider failures so middleware and callers can
// react without parsing provider-specific messages.
type ErrorKind string

const (
	ErrRateLimited    ErrorKind = "rate_limited"     // 429 — retryable, honor Retry-After
	ErrServer         ErrorKind = "server"           // 5xx — retryable
	ErrNetwork        ErrorKind = "network"          // transport failed — retryable
	ErrAuth           ErrorKind = "auth"             // 401/403 — not retryable
	ErrBadRequest     ErrorKind = "bad_request"      // 4xx — not retryable (our bug or bad config)
	ErrOverloaded     ErrorKind = "overloaded"       // provider capacity (Anthropic 529) — retryable
	ErrModelNotFound  ErrorKind = "model_not_found"  // wrong/unknown model id — not retryable
	ErrContextTooLong ErrorKind = "context_too_long" // input exceeds the context window — not retryable
)

// Error is the normalized failure returned by every adapter.
type Error struct {
	Kind       ErrorKind
	Code       string // provider error code when parsed: "model_not_found", "context_length_exceeded", …
	Message    string // human message extracted from the provider's body, when parseable
	StatusCode int    // HTTP status, 0 for network errors
	RetryAfter int    // seconds, from Retry-After if the provider sent one
	Provider   string // "anthropic" | "openai" | ...
	Body       string // truncated raw body — fallback detail when nothing parsed
}

func (e *Error) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Body
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Provider, e.StatusCode, e.Kind, detail)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Kind, detail)
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

// AsError extracts the normalized *Error from an error chain, if present.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// classifyStatus is the status-only fallback when the body tells us nothing.
func classifyStatus(status int) ErrorKind {
	switch {
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusNotFound:
		return ErrModelNotFound
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

// providerErrorBody mirrors both error JSON shapes in the wild:
//
//	anthropic: {"type":"error","error":{"type":"not_found_error","message":"…"}}
//	openai:    {"error":{"message":"…","type":"invalid_request_error","code":"model_not_found"}}
type providerErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// classifyProviderError refines the kind using the parsed body. Signals:
// the code/type strings and well-known phrases in the message.
func classifyProviderError(kind ErrorKind, code, message string) ErrorKind {
	needle := strings.ToLower(code + " " + message + " ")
	switch {
	case strings.Contains(needle, "model_not_found") ||
		strings.Contains(needle, "not_found_error") && strings.Contains(needle, "model"):
		return ErrModelNotFound
	case strings.Contains(needle, "context_length_exceeded") ||
		strings.Contains(needle, "context length") ||
		strings.Contains(needle, "prompt is too long") ||
		strings.Contains(needle, "maximum context length"):
		return ErrContextTooLong
	case strings.Contains(needle, "rate_limit"):
		return ErrRateLimited
	case strings.Contains(needle, "overloaded"):
		return ErrOverloaded
	case strings.Contains(needle, "authentication") ||
		strings.Contains(needle, "invalid api key") ||
		strings.Contains(needle, "invalid_api_key") ||
		strings.Contains(needle, "incorrect api key"):
		return ErrAuth
	default:
		return kind
	}
}

// parseProviderError builds the normalized Error from a non-2xx response.
// It prefers parsed provider JSON over raw body snippets.
func parseProviderError(provider string, resp *http.Response, body []byte) *Error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 512 {
		snippet = snippet[:512]
	}
	e := &Error{
		Kind:       classifyStatus(resp.StatusCode),
		StatusCode: resp.StatusCode,
		RetryAfter: retryAfterSeconds(resp.Header),
		Provider:   provider,
		Body:       snippet,
	}
	var parsed providerErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && (parsed.Error.Message != "" || parsed.Error.Type != "") {
		e.Message = parsed.Error.Message
		e.Code = parsed.Error.Code
		if e.Code == "" {
			e.Code = parsed.Error.Type
		}
		e.Kind = classifyProviderError(e.Kind, e.Code, e.Message)
	}
	return e
}
