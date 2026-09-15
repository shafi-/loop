package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicComplete(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "hello from claude"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 12, "output_tokens": 7}
		}`))
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "sk-test", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model:  "claude-sonnet-4-5",
		System: "be brief",
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, Content: "hello"},
			{Role: RoleUser, Content: "bye"},
		},
		MaxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" || gotKey != "sk-test" || gotVersion != "2023-06-01" {
		t.Errorf("wire: path=%q key=%q ver=%q", gotPath, gotKey, gotVersion)
	}
	if gotBody["model"] != "claude-sonnet-4-5" || gotBody["system"] != "be brief" || gotBody["max_tokens"] != float64(100) {
		t.Errorf("body: %v", gotBody)
	}
	if msgs, _ := gotBody["messages"].([]any); len(msgs) != 3 {
		t.Errorf("system must not appear in messages, got %d", len(msgs))
	}
	if resp.Text != "hello from claude" || resp.StopReason != StopEndTurn ||
		resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Errorf("mapping: %+v", resp)
	}
}

func TestAnthropicToolRoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{
			"content": [{"type": "tool_use", "id": "toolu_1", "name": "read_file",
				"input": {"path": "main.go"}}],
			"stop_reason": "tool_use"
		}`))
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "k", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: RoleUser, Content: "read main.go"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "read_file", Args: `{"path":"main.go"}`}}},
			{Role: RoleTool, ToolCallID: "toolu_1", Name: "read_file", Content: "package main"},
		},
		Tools: []ToolDef{{Name: "read_file", Description: "read a file", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool call mapping: %+v", resp)
	}
	// The prior tool result must appear as a tool_result block addressed to toolu_1.
	msgs, _ := gotBody["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool results travel as user messages, got role %v", last["role"])
	}
	blocks, _ := last["content"].([]any)
	tb := blocks[0].(map[string]any)
	if tb["type"] != "tool_result" || tb["tool_use_id"] != "toolu_1" || tb["content"] != "package main" {
		t.Errorf("tool_result block wrong: %v", tb)
	}
}

func TestAnthropicStructuredUsesForcedTool(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{
			"content": [{"type": "tool_use", "id": "t1", "name": "respond",
				"input": {"speak": true, "priority": 3}}],
			"stop_reason": "tool_use"
		}`))
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "k", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model:          "claude-sonnet-4-5",
		Messages:       []Message{{Role: RoleUser, Content: "decide"}},
		ResponseSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "respond" {
		t.Fatalf("schema should become a forced tool: %v", gotBody["tools"])
	}
	choice := gotBody["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "respond" {
		t.Errorf("tool_choice = %v", choice)
	}
	// The tool arguments become the response text (the JSON itself).
	if resp.Text == "" || strings.Contains(resp.Text, "tool_use") {
		t.Errorf("structured text = %q", resp.Text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &parsed); err != nil || parsed["speak"] != true {
		t.Errorf("structured text should be the schema JSON, got %q (%v)", resp.Text, err)
	}
}

func TestAnthropicStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream flag not sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
			"event: content_block_start\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
			"event: message_delta\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
			"event: message_stop\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "k", BaseURL: srv.URL}
	var got string
	resp, err := p.Stream(context.Background(), Request{Model: "claude-sonnet-4-5"}, func(d string) { got += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello" || resp.Text != "Hello" {
		t.Errorf("deltas=%q text=%q", got, resp.Text)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 3 {
		t.Errorf("usage from stream: %+v", resp.Usage)
	}
}

func TestAnthropicRateLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	p := &Anthropic{APIKey: "k", BaseURL: srv.URL}
	_, err := p.Complete(context.Background(), Request{Model: "claude-sonnet-4-5"})
	e, ok := AsError(err)
	if !ok || e.Kind != ErrRateLimited || e.RetryAfter != 7 || !e.Retryable() {
		t.Fatalf("want retryable rate limit with Retry-After=7, got %#v (%v)", e, err)
	}
}

func TestOpenAIComplete(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hello from gpt"},
				"finish_reason": "stop"}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 9}
		}`))
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "sk-oai", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model: "llama-3.3-70b", System: "be brief", MaxTokens: 55,
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer sk-oai" {
		t.Errorf("wire: path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["max_tokens"] != float64(55) {
		t.Errorf("non-reasoning models use max_tokens, got %v", gotBody["max_tokens"])
	}
	msgs, _ := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("system must be the first message, got %v", first)
	}
	if resp.Text != "hello from gpt" || resp.StopReason != StopEndTurn || resp.Usage.OutputTokens != 9 {
		t.Errorf("mapping: %+v", resp)
	}
}

func TestOpenAIReasoningModelUsesCompletionTokens(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "k", BaseURL: srv.URL}
	if _, err := p.Complete(context.Background(), Request{Model: "gpt-5", MaxTokens: 99}); err != nil {
		t.Fatal(err)
	}
	if gotBody["max_completion_tokens"] != float64(99) {
		t.Errorf("gpt-5 should use max_completion_tokens, got %v", gotBody)
	}
	if _, present := gotBody["max_tokens"]; present {
		t.Error("gpt-5 must not send max_tokens")
	}
}

func TestOpenAIStructuredWithFallback(t *testing.T) {
	var formats []bool // whether each request carried response_format
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, has := body["response_format"]
		formats = append(formats, has)
		if len(formats) == 1 {
			// First attempt: server rejects response_format.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"speak\":false}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "k", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model:          "llama-3.3-70b",
		Messages:       []Message{{Role: RoleUser, Content: "decide"}},
		ResponseSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(formats) != 2 || formats[0] != true || formats[1] != false {
		t.Fatalf("expected fallback retry without response_format, saw %v", formats)
	}
	if !strings.Contains(resp.Text, "speak") {
		t.Errorf("structured text = %q", resp.Text)
	}
}

func TestOpenAIStreamWithToolCallAccumulation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Error("stream flag not sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"think\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "k", BaseURL: srv.URL}
	var got string
	resp, err := p.Stream(context.Background(), Request{Model: "llama-3.3-70b"}, func(d string) { got += d })
	if err != nil {
		t.Fatal(err)
	}
	if got != "think" || resp.Text != "think" {
		t.Errorf("text deltas = %q", got)
	}
	if resp.StopReason != StopToolUse || len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", resp)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || tc.Args != `{"path":"a.go"}` {
		t.Errorf("accumulated tool call = %+v", tc)
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	if _, err := New("gemini", Options{}); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
}

// TestOpenAIToolRoundTrip pins the OpenAI-family tool mapping both ways:
// ToolDef → wire tools, assistant ToolCalls + tool results → wire
// messages, and response tool_calls → Response.ToolCalls. The agent
// tool loop (room agents with tools) depends on this path; it must
// work identically to the Anthropic family.
func TestOpenAIToolRoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "",
				"tool_calls": [{"id": "call_1", "type": "function",
					"function": {"name": "write_file", "arguments": "{\"path\":\"plans/x.md\",\"content\":\"# x\"}"}}]},
				"finish_reason": "tool_calls"}]
		}`))
	}))
	defer srv.Close()

	p := &OpenAI{APIKey: "sk-oai", BaseURL: srv.URL}
	resp, err := p.Complete(context.Background(), Request{
		Model: "llama-3.3-70b",
		Tools: []ToolDef{{
			Name:        "write_file",
			Description: "Write a file",
			Schema:      map[string]any{"type": "object"},
		}},
		Messages: []Message{
			{Role: RoleUser, Content: "persist the plan"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "read_file", Args: `{"path":"notes.md"}`}}},
			{Role: RoleTool, ToolCallID: "call_0", Name: "read_file", Content: "prior notes"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request side: tools array + faithful message history.
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tt := tools[0].(map[string]any)
	if tt["type"] != "function" {
		t.Errorf("tool type = %v", tt["type"])
	}
	fn := tt["function"].(map[string]any)
	if fn["name"] != "write_file" || fn["description"] != "Write a file" {
		t.Errorf("tool function = %v", fn)
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	c0 := calls[0].(map[string]any)
	if c0["id"] != "call_0" || c0["function"].(map[string]any)["name"] != "read_file" {
		t.Errorf("assistant tool_calls = %v", calls)
	}
	if args := c0["function"].(map[string]any)["arguments"]; args != `{"path":"notes.md"}` {
		t.Errorf("arguments must be the raw JSON string, got %v", args)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_0" || toolMsg["content"] != "prior notes" {
		t.Errorf("tool result message = %v", toolMsg)
	}

	// Response side: parsed tool call with raw JSON args.
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "write_file" {
		t.Errorf("tool call = %+v", tc)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Args), &args); err != nil || args["path"] != "plans/x.md" {
		t.Errorf("args = %q (%v)", tc.Args, err)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop reason = %v", resp.StopReason)
	}
}
