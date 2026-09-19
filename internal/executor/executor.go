// Package executor defines the boundary between loop's deterministic
// orchestration and the engines that actually run agentic tasks. An
// executor receives a fully-resolved task (instruction, persona, model,
// tool policy) and drives its own internal LLM/tool loop, streaming
// events back for the run log. loop never inspects the executor's
// internals — auditability comes from the event stream, not from owning
// the loop.
//
// v1 ships the "cline" executor (Node sidecar over @cline/sdk, M3). Its
// tool policy binds end-to-end: the task's tools select the agent's
// toolset, and approval=ask routes gated calls (shell commands) to the
// task's AskApproval callback over the protocol. loop's own micro-LLM
// calls (narrator, speak-policy, router classification) do NOT go
// through executors; they use internal/llm.
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

	// AskApproval is consulted for tool calls the approval policy gates
	// (approval: "ask") — in practice shell commands; read and write
	// tools stay auto-approved. tool names the executor-side tool and
	// detail renders the call for a human (the command line for shell
	// tools). An error from the callback denies the call with that
	// reason. nil = nothing can be approved (calls are denied).
	AskApproval func(ctx context.Context, tool, detail string) (approved bool, reason string, err error)
}

// EventType discriminates executor stream events.
type EventType string

const (
	EventText       EventType = "text"      // assistant text delta
	EventToolCall   EventType = "tool_call" // executor-side tool invocation
	EventToolResult EventType = "tool_result"
	EventNotice     EventType = "notice" // executor lifecycle commentary
	EventUsage      EventType = "usage"  // cumulative provider tokens so far
)

// Event is one observation from a running executor.
type Event struct {
	Type   EventType
	Text   string
	Tool   string
	Detail string
	// InputTokens/OutputTokens carry EventUsage: the executor-side
	// provider totals so far (the executor owns the key and the call,
	// so this is how its spend reaches loop's meter).
	InputTokens  int
	OutputTokens int
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
