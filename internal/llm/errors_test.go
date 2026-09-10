package llm

import (
	"net/http"
	"strings"
	"testing"
)

func respWithStatus(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header}
}

func TestParseAnthropicModelNotFound(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-sonnet-99"}}`)
	e := parseProviderError("anthropic", respWithStatus(404, string(body), nil), body)
	if e.Kind != ErrModelNotFound {
		t.Errorf("kind = %q", e.Kind)
	}
	if e.Message != "model: claude-sonnet-99" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Code != "not_found_error" {
		t.Errorf("code = %q", e.Code)
	}
	if e.Retryable() {
		t.Error("model not found must not retry")
	}
	// The rendered error should carry the human message, not raw JSON.
	if !strings.Contains(e.Error(), "claude-sonnet-99") || strings.Contains(e.Error(), "{\"type\"") {
		t.Errorf("rendered = %q", e.Error())
	}
}

func TestParseOpenAIContextTooLong(t *testing.T) {
	body := []byte(`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	e := parseProviderError("openai", respWithStatus(400, string(body), nil), body)
	if e.Kind != ErrContextTooLong {
		t.Errorf("kind = %q", e.Kind)
	}
	if h := Hint(e); !strings.Contains(h, "context window") {
		t.Errorf("hint = %q", h)
	}
}

func TestParseAuthShapes(t *testing.T) {
	anthropic := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	e1 := parseProviderError("anthropic", respWithStatus(401, string(anthropic), nil), anthropic)
	if e1.Kind != ErrAuth {
		t.Errorf("anthropic auth kind = %q", e1.Kind)
	}

	openai := []byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
	e2 := parseProviderError("openai", respWithStatus(401, string(openai), nil), openai)
	if e2.Kind != ErrAuth {
		t.Errorf("openai auth kind = %q", e2.Kind)
	}
}

func TestParseRateLimitAndOverload(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "12")
	anthropic := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)
	e := parseProviderError("anthropic", respWithStatus(429, string(anthropic), h), anthropic)
	if e.Kind != ErrRateLimited || !e.Retryable() || e.RetryAfter != 12 {
		t.Errorf("rate limit = %+v", e)
	}

	overload := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	e2 := parseProviderError("anthropic", respWithStatus(529, string(overload), nil), overload)
	if e2.Kind != ErrOverloaded || !e2.Retryable() {
		t.Errorf("overloaded = %+v", e2)
	}
}

func TestParseGarbageBodyFallsBackToStatus(t *testing.T) {
	e := parseProviderError("openai", respWithStatus(500, "<html>gateway oops</html>", nil), []byte("<html>gateway oops</html>"))
	if e.Kind != ErrServer || !e.Retryable() {
		t.Errorf("garbage body = %+v", e)
	}
	if !strings.Contains(e.Error(), "gateway oops") {
		t.Errorf("body snippet lost: %q", e.Error())
	}
}

func TestRetryExhaustionAnnotated(t *testing.T) {
	f := &flakyProvider{failures: 100, err: &Error{Kind: ErrRateLimited, Provider: "x"}}
	_, err := WithRetry(f, 3, 1).Complete(t.Context(), Request{})
	if err == nil || !strings.Contains(err.Error(), "still failing after 3 attempts") {
		t.Errorf("exhaustion = %v", err)
	}
}

func TestHintUnknownKindIsEmpty(t *testing.T) {
	if h := Hint(errPlain("nope")); h != "" {
		t.Errorf("hint for non-llm error = %q", h)
	}
}

type plainErr string

func (p plainErr) Error() string { return string(p) }

func errPlain(s string) error { return plainErr(s) }
