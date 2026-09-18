package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Anthropic implements Provider against the native Anthropic Messages API
// (https://docs.anthropic.com/en/api/messages).
type Anthropic struct {
	// APIKey comes from the environment (ANTHROPIC_API_KEY by default).
	APIKey string
	// BaseURL overrides https://api.anthropic.com (used by tests).
	BaseURL string
}

const anthropicDefaultMaxTokens = 4096

// Provider name for errors and registry lookup.
func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) endpoint() string {
	base := "https://api.anthropic.com"
	if a.BaseURL != "" {
		base = a.BaseURL
	}
	// loop's convention (shared with ANTHROPIC_BASE_URL providers like
	// Z.ai): the base carries no /v1 — requests go to <base>/v1/messages.
	// Tolerate a base that already ends in /v1 or a trailing slash so the
	// prefix never doubles; the /v1/messages suffix is added at the call
	// sites. Mirrors the cline host's normalization for the same SDK
	// boundary.
	base = strings.TrimRight(base, "/")
	return strings.TrimSuffix(base, "/v1")
}

// anthropicRequest is the Messages API wire format.
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  *anthropicChoice   `json:"tool_choice,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicBlock
}

type anthropicBlock struct {
	Type string `json:"type"` // text | tool_use | tool_result

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicChoice struct {
	Type string `json:"type"` // auto | any | tool
	Name string `json:"name,omitempty"`
}

// anthropicResponse is the non-streaming wire response.
type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) headers() http.Header {
	h := http.Header{}
	h.Set("x-api-key", a.APIKey)
	h.Set("anthropic-version", "2023-06-01")
	return h
}

// toWire converts a normalized Request into Anthropic's shape.
func (a *Anthropic) toWire(req Request, stream bool) (*anthropicRequest, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = anthropicDefaultMaxTokens
	}
	wire := &anthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		System:      req.System,
		Temperature: req.Temperature,
		Stream:      stream,
	}

	structured := req.ResponseSchema != nil
	if structured {
		// Forced-tool pattern: the schema becomes a tool the model must call.
		wire.Tools = append(wire.Tools, anthropicTool{
			Name:        "respond",
			Description: "Provide your final response as JSON matching the schema.",
			InputSchema: req.ResponseSchema,
		})
		wire.ToolChoice = &anthropicChoice{Type: "tool", Name: "respond"}
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
		})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			wire.Messages = append(wire.Messages, anthropicMessage{Role: "user", Content: m.Content})
		case RoleAssistant:
			am := anthropicMessage{Role: "assistant"}
			if m.Content != "" {
				am.Content = []anthropicBlock{{Type: "text", Text: m.Content}}
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Args)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks, _ := am.Content.([]anthropicBlock)
				am.Content = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if am.Content == nil {
				am.Content = ""
			}
			wire.Messages = append(wire.Messages, am)
		case RoleTool:
			// Tool results travel as user messages containing tool_result blocks.
			wire.Messages = append(wire.Messages, anthropicMessage{Role: "user", Content: []anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}}})
		case RoleSystem:
			return nil, fmt.Errorf("system role belongs in Request.System, not Messages")
		}
	}
	return wire, nil
}

func (a *Anthropic) fromWire(r *anthropicResponse) *Response {
	out := &Response{StopReason: mapStop(r.StopReason), Usage: Usage{
		InputTokens:  r.Usage.InputTokens,
		OutputTokens: r.Usage.OutputTokens,
	}}
	var text []byte
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text...)
		case "tool_use":
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Args: string(args)})
		}
	}
	out.Text = string(text)
	return out
}

func mapStop(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	default:
		return StopOther
	}
}

// Complete implements Provider.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	wire, err := a.toWire(req, false)
	if err != nil {
		return nil, err
	}
	var raw anthropicResponse
	if err := postJSON(ctx, a.Name(), a.endpoint()+"/v1/messages", a.headers(), wire, &raw); err != nil {
		return nil, err
	}
	resp := a.fromWire(&raw)
	// Structured mode: the "respond" tool's arguments are the answer text.
	if req.ResponseSchema != nil && len(resp.ToolCalls) == 1 && resp.ToolCalls[0].Name == "respond" {
		resp.Text = resp.ToolCalls[0].Args
		resp.ToolCalls = nil
	}
	return resp, nil
}

// Stream implements Provider via SSE.
func (a *Anthropic) Stream(ctx context.Context, req Request, onDelta StreamFunc) (*Response, error) {
	// Structured output arrives as tool arguments, not text: no stream to show.
	if req.ResponseSchema != nil {
		resp, err := a.Complete(ctx, req)
		if err == nil && onDelta != nil {
			onDelta(resp.Text)
		}
		return resp, err
	}

	wire, err := a.toWire(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := a.openStream(ctx, wire, onDelta)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
