// Package executor defines the boundary between loop's deterministic
// orchestration and the engines that actually run agentic tasks. An
// executor receives a fully-resolved task (instruction, persona, model,
// tool policy) and drives its own internal LLM/tool loop, streaming
// events back for the run log. loop never inspects the executor's
// internals — auditability comes from the event stream, not from owning
// the loop.
//
// v1 ships the "cline" executor (Node sidecar over @cline/sdk, M3).
// loop's own micro-LLM calls (narrator, speak-policy, router
// classification) do NOT go through executors; they use internal/llm.
package executor

import "context"

// ModelSpec is the resolved model configuration for a task.
type ModelSpec struct {
	Provider    string // "anthropic" | "openai"
	Model       string
	APIKey      string
	BaseURL     string
	Temperature *float64
	MaxTokens   int
}

// Task is a fully-resolved unit of agentic work. Everything is concrete:
// templates are interpolated before an executor ever sees the task.
type Task struct {
	Instruction string
	System      string // persona system prompt, may be empty
	CWD         string // working directory for tool execution
	Model       ModelSpec

	Tools    []string // allowed tool names; empty = executor default
	MaxTurns int      // 0 = executor default
	Approval string   // "auto" | "ask": tool approval policy
}

// EventType discriminates executor stream events.
type EventType string

const (
	EventText       EventType = "text"      // assistant text delta
	EventToolCall   EventType = "tool_call" // executor-side tool invocation
	EventToolResult EventType = "tool_result"
	EventNotice     EventType = "notice" // executor lifecycle commentary
)

// Event is one observation from a running executor.
type Event struct {
	Type   EventType
	Text   string
	Tool   string
	Detail string
}

// Result is the aggregated output of a finished task.
type Result struct {
	Output string // final assistant text
}

// Executor runs one task. Implementations must be safe for concurrent
// use, stream progress via onEvent (may be nil), and return a non-nil
// error on failure.
type Executor interface {
	Name() string
	Run(ctx context.Context, task Task, onEvent func(Event)) (*Result, error)
}

// Registry maps executor names to implementations. The runner consults
// it for agent stages; a missing executor is a hard, explicit error.
type Registry struct {
	executors map[string]Executor
}

func NewRegistry() *Registry { return &Registry{executors: map[string]Executor{}} }

func (r *Registry) Register(e Executor) { r.executors[e.Name()] = e }

func (r *Registry) Get(name string) (Executor, bool) {
	e, ok := r.executors[name]
	return e, ok
}

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.executors))
	for n := range r.executors {
		out = append(out, n)
	}
	return out
}
