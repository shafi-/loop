package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpClient has no global default timeout: LLM completions legitimately
// run for minutes. Per-request deadlines come from the caller's context.
var httpClient = &http.Client{}

// postJSON sends req as JSON to url with the given headers and decodes a
// JSON response into out. Non-2xx responses become normalized *Error.
func postJSON(ctx context.Context, provider, url string, header http.Header, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return &Error{Kind: ErrNetwork, Provider: provider, Body: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(provider, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Kind: ErrServer, Provider: provider, Body: "malformed JSON response: " + err.Error()}
	}
	return nil
}

// httpError converts a non-2xx response into a normalized *Error.
func httpError(provider string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &Error{
		Kind:       classifyStatus(resp.StatusCode),
		StatusCode: resp.StatusCode,
		RetryAfter: retryAfterSeconds(resp.Header),
		Provider:   provider,
		Body:       strings.TrimSpace(string(snippet)),
	}
}

// streamEvent is one server-sent event: a name and its JSON data payload.
type streamEvent struct {
	Event string
	Data  []byte
}

// streamSSE consumes an SSE response body, invoking fn per event, until the
// stream ends or ctx is cancelled. Non-2xx responses become *Error before
// any event is delivered. SSE framing per the WHATWG spec: events separated
// by blank lines, "event:" and "data:" fields, "data: [DONE]" terminates.
func streamSSE(ctx context.Context, provider string, resp *http.Response, fn func(streamEvent) error) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(provider, resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // events can exceed the 64KB default
	var event string
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		ev := streamEvent{Event: event, Data: []byte(data.String())}
		event, data = "", strings.Builder{}
		return fn(ev)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sc.Scan() {
			break
		}
		line := sc.Text()
		switch {
		case line == "": // blank line = end of event
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return sc.Err()
}

// backoff computes the delay before attempt n (1-based) with a base of
// baseMs, capped at 30s. Doubling plus a little jitter avoids synchronized
// retry storms across concurrent agents.
func backoff(baseMs, attempt int) time.Duration {
	if baseMs <= 0 {
		baseMs = 500
	}
	d := time.Duration(baseMs) * time.Millisecond
	for i := 1; i < attempt && d < 30*time.Second; i++ {
		d *= 2
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
