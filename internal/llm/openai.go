package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OpenAI implements Provider against the OpenAI Chat Completions API shape.
// With a custom BaseURL it speaks to any compatible service: Groq, Ollama,
// OpenRouter, Together, DeepSeek, vLLM, LM Studio, and friends.
type OpenAI struct {
	APIKey  string
	BaseURL string // e.g. http://localhost:11434/v1; empty = api.openai.com/v1
}

const openaiDefaultMaxTokens = 4096

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) endpoint() string {
	if o.BaseURL != "" {
		return strings.TrimSuffix(o.BaseURL, "/")
	}
	return "https://api.openai.com/v1"
}

// openaiMessage is the Chat Completions wire format.
type openaiMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []openaiToolUse `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type openaiToolUse struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON string, not an object
	} `json:"function"`
}

type openaiTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

type openaiRequest struct {
	Model          string          `json:"model"`
	Messages       []openaiMessage `json:"messages"`
	Tools          []openaiTool    `json:"tools,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	MaxCompletion  int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat *openaiFormat   `json:"response_format,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
}

// openaiFormat is the structured-output directive. Strict schemas are the
// OpenAI feature; compatible servers either honor or (we fall back) reject it.
type openaiFormat struct {
	Type       string `json:"type"` // "json_schema"
	JSONSchema struct {
		Name   string         `json:"name"`
		Schema map[string]any `json:"schema"`
		Strict bool           `json:"strict"`
	} `json:"json_schema"`
}

type openaiResponse struct {
	Choices []struct {
		Message      openaiMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// needsCompletionTokens reports whether the model requires the newer
// max_completion_tokens parameter (OpenAI reasoning models and gpt-5
// generation). Everything else in the ecosystem still speaks max_tokens.
func needsCompletionTokens(model string) bool {
	m := strings.ToLower(model)
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

func (o *OpenAI) toWire(req Request, structured bool) *openaiRequest {
	wire := &openaiRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
	}
	if req.MaxTokens > 0 {
		if needsCompletionTokens(req.Model) {
			wire.MaxCompletion = req.MaxTokens
		} else {
			wire.MaxTokens = req.MaxTokens
		}
	} else if needsCompletionTokens(req.Model) {
		wire.MaxCompletion = openaiDefaultMaxTokens
	} else {
		wire.MaxTokens = openaiDefaultMaxTokens
	}

	if req.System != "" {
		wire.Messages = append(wire.Messages, openaiMessage{Role: "system", Content: req.System})
	}
	if structured {
		f := &openaiFormat{Type: "json_schema"}
		f.JSONSchema.Name = "respond"
		f.JSONSchema.Schema = req.ResponseSchema
		f.JSONSchema.Strict = true
		wire.ResponseFormat = f
	}
	for _, t := range req.Tools {
		ot := openaiTool{Type: "function"}
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Schema
		wire.Tools = append(wire.Tools, ot)
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			wire.Messages = append(wire.Messages, openaiMessage{Role: "user", Content: m.Content})
		case RoleAssistant:
			om := openaiMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, openaiToolUseFrom(tc))
			}
			wire.Messages = append(wire.Messages, om)
		case RoleTool:
			wire.Messages = append(wire.Messages, openaiMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: m.Content})
		case RoleSystem:
			wire.Messages = append(wire.Messages, openaiMessage{Role: "system", Content: m.Content})
		}
	}
	return wire
}

func openaiToolUseFrom(tc ToolCall) openaiToolUse {
	var t openaiToolUse
	t.ID = tc.ID
	t.Type = "function"
	t.Function.Name = tc.Name
	t.Function.Arguments = tc.Args
	if t.Function.Arguments == "" {
		t.Function.Arguments = "{}"
	}
	return t
}

func mapOpenAIFinish(s string) StopReason {
	switch s {
	case "stop":
		return StopEndTurn
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxTokens
	default:
		return StopOther
	}
}

func (o *OpenAI) fromWire(r *openaiResponse) *Response {
	out := &Response{
		StopReason: mapOpenAIFinish(r.Choices[0].FinishReason),
		Usage:      Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens},
	}
	msg := r.Choices[0].Message
	out.Text = msg.Content
	for _, tc := range msg.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	return out
}

// schemaFallback wraps structured output for servers without response_format
// support: the schema travels as an instruction instead.
func schemaInstruction(schema map[string]any) string {
	raw, _ := json.Marshal(schema)
	return fmt.Sprintf("Respond with ONLY a JSON value matching this JSON Schema. No prose, no markdown fences.\n\n%s", raw)
}

// Complete implements Provider.
func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	structured := req.ResponseSchema != nil
	resp, err := o.complete(ctx, req, structured)
	if err != nil && structured {
		// Long-tail compatibility: some servers reject response_format. Retry
		// once with the schema embedded in the prompt instead.
		if e, ok := AsError(err); ok && e.Kind == ErrBadRequest {
			return o.completeWithSchemaFallback(ctx, req)
		}
	}
	return resp, err
}

func (o *OpenAI) complete(ctx context.Context, req Request, structured bool) (*Response, error) {
	wire := o.toWire(req, structured)
	var raw openaiResponse
	if err := postJSON(ctx, o.Name(), o.endpoint()+"/chat/completions", o.headers(), wire, &raw); err != nil {
		return nil, err
	}
	if len(raw.Choices) == 0 {
		return nil, &Error{Kind: ErrServer, Provider: o.Name(), Body: "response contained no choices"}
	}
	return o.fromWire(&raw), nil
}

func (o *OpenAI) completeWithSchemaFallback(ctx context.Context, req Request) (*Response, error) {
	req2 := req
	req2.ResponseSchema = nil
	req2.System = strings.TrimSpace(req2.System + "\n\n" + schemaInstruction(req.ResponseSchema))
	return o.complete(ctx, req2, false)
}

func (o *OpenAI) headers() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+o.APIKey)
	return h
}

// Stream implements Provider via SSE. Structured output streams as one
// delta (it arrives as JSON, not incremental prose).
func (o *OpenAI) Stream(ctx context.Context, req Request, onDelta StreamFunc) (*Response, error) {
	if req.ResponseSchema != nil {
		resp, err := o.Complete(ctx, req)
		if err == nil && onDelta != nil {
			onDelta(resp.Text)
		}
		return resp, err
	}

	wire := o.toWire(req, false)
	wireStream := *wire
	wireStream.Stream = true
	return o.stream(ctx, &wireStream, onDelta)
}
