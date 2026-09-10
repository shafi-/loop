package llm

import "context"

// Role identifies who authored a message in a conversation.
type Role string

const (
	RoleSystem   Role = "system"
	RoleUser     Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool     Role = "tool" // a tool result answering a ToolCall
)

// ToolCall is the model requesting a tool execution (or, in role-tool
// messages, the answer to one).
type ToolCall struct {
	ID   string
	Name string
	Args string // raw JSON arguments object
}

// Message is one turn in a conversation. Content carries text; ToolCalls
// is set on assistant messages that request tools; ToolCallID and Name are
// set on tool-result messages.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

// ToolDef describes a callable tool in provider-neutral JSON Schema form.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema for the arguments object
}

// Request is a normalized completion request across providers.
type Request struct {
	Model    string
	System   string    // rendered as a system prompt (Anthropic) / first message (OpenAI)
	Messages []Message // conversation; must not include the system turn
	Tools    []ToolDef

	// ResponseSchema, when set, asks the provider for JSON output conforming
	// to this JSON Schema as the assistant message content. Streaming is
	// degraded to plain text accumulation when a schema is set.
	ResponseSchema map[string]any

	Temperature *float64 // nil = provider default
	MaxTokens   int      // 0 = adapter default
}

// StopReason explains why the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"     // model finished
	StopToolUse   StopReason = "tool_use"     // model wants tools executed
	StopMaxTokens StopReason = "max_tokens"   // ran out of budget
	StopOther     StopReason = "other"
)

// Usage reports token consumption for one request.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the aggregated result of a completion (streamed or not).
type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      Usage
}

// StreamFunc receives text deltas as they arrive. It may be nil when the
// caller does not want incremental output.
type StreamFunc func(delta string)

// Provider is the single interface every LLM backend implements. Complete
// blocks until the full response is ready; Stream invokes onDelta per text
// chunk and returns the same aggregate Response Complete would.
//
// Implementations must be safe for concurrent use.
type Provider interface {
	Complete(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request, onDelta StreamFunc) (*Response, error)
}
