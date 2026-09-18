// Package agent implements the runtime behind room personas: direct
// replies (streamed) and the speak-or-stay-silent decision that makes
// untagged agents behave like busy senior people rather than
// round-robin chatterbots.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// Agent is one room participant: a persona with a resolved provider.
type Agent struct {
	Persona  config.Persona
	Provider llm.Provider
	// CWD confines the persona's file tools (empty = process cwd).
	CWD string
	// Workspace is the project brief grounding the persona in the
	// workspace the room was opened in (workspace.Brief). Empty = no
	// brief; prompts are then identical to a brief-less agent.
	Workspace string
	// Knowledge returns the maintained project digest block
	// (knowledge.DigestBlock), re-read per reply so an auto-refresh
	// mid-session is seen by the very next turn. nil = no knowledge
	// layer; the decision prompt deliberately never sees it.
	Knowledge func() string
	// ToolHook, when set, is called once per executed tool with a
	// compact human-readable line ("wrote plans/x.md (120 bytes)") —
	// the room wires it to a UI notice and a transcript line.
	ToolHook func(name, detail string)
}

// Name returns the persona identifier.
func (a *Agent) Name() string { return a.Persona.Name }

// Model returns the resolved model id this agent's requests use.
func (a *Agent) Model() string { return a.model() }

// Decision is the structured outcome of the speak-or-silent call.
type Decision struct {
	Speak    bool   `json:"speak"`
	Priority int    `json:"priority"` // 1..5; 5 = must respond
	Reason   string `json:"reason"`
}

// Confidence normalizes priority to 0..1 for threshold comparison
// (threshold 0.6 → priority 4+ qualifies).
func (d Decision) Confidence() float64 {
	if d.Priority < 1 {
		return 0
	}
	if d.Priority > 5 {
		d.Priority = 5
	}
	return float64(d.Priority-1) / 4
}

// speakSchema is the structured-output contract for decisions. Every
// field is required so OpenAI strict mode accepts it; the response
// arrives as JSON text via either provider's structured path.
var speakSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"speak":    map[string]any{"type": "boolean"},
		"priority": map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
		"reason":   map[string]any{"type": "string"},
	},
	"required":             []string{"speak", "priority", "reason"},
	"additionalProperties": false,
}

// others lists the rest of the room for framing prompts.
func others(room []config.Persona, self string) string {
	var names []string
	for _, p := range room {
		if p.Name != self {
			names = append(names, p.Name)
		}
	}
	return strings.Join(names, ", ")
}

// framing is the shared room-context preface for this persona.
func (a *Agent) framing(room []config.Persona) string {
	role := a.Persona.Role
	if role == "" {
		role = "colleague"
	}
	return fmt.Sprintf("You are %s, the %s, in a strategy room with: %s. The user you are speaking to founded this room.",
		a.Persona.Name, role, others(room, a.Persona.Name))
}

// base composes the persona's identity preface: its own system prompt,
// the workspace brief (when the host supplied one), the maintained
// project digest (same terms), and the room framing. Both reply paths
// build on it so grounding can never reach one and miss the other.
func (a *Agent) base(room []config.Persona) string {
	parts := make([]string, 0, 4)
	if s := strings.TrimSpace(a.Persona.System); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(a.Workspace); s != "" {
		parts = append(parts, s)
	}
	if a.Knowledge != nil {
		if s := strings.TrimSpace(a.Knowledge()); s != "" {
			parts = append(parts, s)
		}
	}
	parts = append(parts, a.framing(room))
	return strings.Join(parts, "\n\n")
}

// Reply produces this agent's answer to the conversation. The transcript
// is explicitly attributed ([name] lines) because provider role arrays
// cannot represent multi-party conversations cleanly. When onDelta is
// non-nil the reply streams through it as it is generated.
//
// Personas with tools get a bounded agentic loop instead of a single
// completion: the model may call read_file / write_file / run_command
// (confined to the workspace) until it produces its final text.
func (a *Agent) Reply(ctx context.Context, room []config.Persona, conversation string, onDelta llm.StreamFunc) (string, error) {
	if len(a.Persona.Tools) > 0 {
		return a.replyWithTools(ctx, room, conversation, onDelta)
	}
	system := strings.TrimSpace(a.base(room) + `
Reply to the user directly. Stay strictly in your role's perspective.
Be concise. Do not repeat what other participants already said. Never
prefix your reply with your own name.`)
	req := llm.Request{
		Model:    a.model(),
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: conversation}},
	}
	resp, err := a.Provider.Stream(ctx, req, onDelta)
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// workspaceOrientation is appended to tool-using room agents: it names
// the run-record conventions so an agent asked "what's the status?"
// reads the real files instead of guessing filenames until it exhausts
// its tool budget.
const workspaceOrientation = `
Workspace orientation: pipeline runs keep their records under
.loop/runs/<run-id>/ — state.json (the run's status: done, failed, or
paused), events.jsonl (everything that happened, in order), and
context.json (every stage's output text). To report a run's status,
read its state.json first, then the tail of events.jsonl. Do not
guess other filenames. A maintained digest of this codebase lives
under .loop/knowledge/ (you may already see it above): for orientation
prefer the project_notes tool — no argument lists the area notes, a
slug reads one — and open files with read_file only when you need
exact code.`

// replyWithTools is the native tool loop for room agents: rounds of
// completions with tools available, tool calls executed in-process
// (path-guarded, output-capped), until the model answers in text or
// the round budget is spent. Tool rounds use Complete — the final text
// is delivered as one delta.
func (a *Agent) replyWithTools(ctx context.Context, room []config.Persona, conversation string, onDelta llm.StreamFunc) (string, error) {
	system := strings.TrimSpace(a.base(room) + `
Reply to the user directly. Stay strictly in your role's perspective.
Be concise. Do not repeat what other participants already said. Never
prefix your reply with your own name.

You have tools: read_file, write_file, run_command — confined to the
workspace. When a deliverable is worth keeping (a plan, a brief, a
report), write it to a file and say so in one line. Keep tool use
purposeful; conversation is still your main job.` + workspaceOrientation)

	msgs := []llm.Message{{Role: llm.RoleUser, Content: conversation}}
	for round := 0; round < MaxToolRounds; round++ {
		req := llm.Request{
			Model:    a.model(),
			System:   system,
			Messages: msgs,
			Tools:    toolDefsFor(a.Persona.Tools),
		}
		resp, err := a.Provider.Complete(ctx, req)
		if err != nil {
			return "", err
		}
		if len(resp.ToolCalls) == 0 {
			if onDelta != nil && resp.Text != "" {
				onDelta(resp.Text)
			}
			return resp.Text, nil
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: resp.Text, ToolCalls: resp.ToolCalls})
		for _, tc := range resp.ToolCalls {
			out, terr := execRoomTool(ctx, tc.Name, tc.Args, a.CWD)
			a.reportTool(tc, out, terr)
			// Tool errors go back to the model as results, not as fatal
			// failures: it can correct a bad path and retry.
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: outOrError(out, terr)})
		}
	}
	return "", fmt.Errorf("%s exceeded %d tool rounds without answering — narrow the request", a.Persona.Name, MaxToolRounds)
}

// reportTool surfaces one executed tool to the room (UI + transcript)
// as a compact, human-readable line.
func (a *Agent) reportTool(tc llm.ToolCall, out string, err error) {
	if a.ToolHook == nil {
		return
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(tc.Args), &args)
	path, _ := args["path"].(string)
	cmd, _ := args["command"].(string)

	var detail string
	switch tc.Name {
	case "write_file":
		detail = "wrote " + path
	case "read_file":
		detail = "read " + path
	case "run_command":
		detail = cmd
	default:
		detail = tc.Name
	}
	if err != nil {
		detail += fmt.Sprintf(" — failed: %v", err)
	}
	a.ToolHook(tc.Name, strings.TrimSpace(detail))
}

// outOrError renders a tool round-trip for the model: errors are
// content, so the model can recover.
func outOrError(out string, err error) string {
	if err != nil {
		if out != "" {
			return out + "\nerror: " + err.Error()
		}
		return "error: " + err.Error()
	}
	return out
}

// decideSpeakPrompt frames the decision. The user is the CEO: their
// message is an invitation to contribute, and the default posture is
// to answer with your role's best perspective — silence is the rare
// case, not the polite one. (The old framing — "silence is
// respectable" — made rooms dead: every untagged message ended in
// silence.) The conversation ends with the CEO's new message; the
// prompt points at it rather than embedding it twice.
func (a *Agent) decideSpeakPrompt(conversation string) string {
	role := a.Persona.Role
	if role == "" {
		role = "colleague"
	}
	return fmt.Sprintf(`The conversation so far — the CEO's new message is its LAST line:

%s
You are %s, the %s. The user is the CEO of the company: when the CEO
speaks, everyone at this table is expected to bring their best
perspective — your role's vantage point is exactly why you are in the
room.

speak: true when the CEO's message deserves an answer — a question, a
proposal, a problem, a decision, news. Bring facts and judgment others
may not have; a crisp, useful answer from your seat beats silence.
speak: false only when you truly have nothing to add: pure
acknowledgements ("thanks", "ok"), or your point was already made by
another agent this turn.

priority: how strongly you should answer. 5 — the CEO asked something
your role is closest to. 4 — you can add real value from your seat.
3 — marginal color only. 1-2 — the silent cases above.`,
		conversation, a.Persona.Name, role)
}

// DecideSpeak asks whether this agent should respond to the new message.
// It uses structured output: cheap (few hundred tokens), typed, and
// identical across providers. The conversation must end with the new
// user message.
func (a *Agent) DecideSpeak(ctx context.Context, room []config.Persona, conversation string) (Decision, error) {
	system := fmt.Sprintf("You are %s, the %s. You decide whether you personally should answer the CEO's latest message, and how strongly.", a.Persona.Name, a.Persona.Role)

	req := llm.Request{
		Model:          a.model(),
		System:         system,
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: a.decideSpeakPrompt(conversation)}},
		ResponseSchema: speakSchema,
		MaxTokens:      300,
	}
	resp, err := a.Provider.Complete(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	var d Decision
	if err := json.Unmarshal([]byte(resp.Text), &d); err != nil {
		return Decision{}, fmt.Errorf("speak decision was not valid JSON (%w); raw: %.120s", err, resp.Text)
	}
	if d.Priority < 1 {
		d.Priority = 1
	}
	if d.Priority > 5 {
		d.Priority = 5
	}
	return d, nil
}

func (a *Agent) model() string {
	// Nil-safe: a persona without a model block is env-driven.
	return a.Persona.Model.Resolve().Model
}
