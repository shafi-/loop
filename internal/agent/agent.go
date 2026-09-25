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
	// compact human-readable line ("wrote .loop/plans/x.md (120 bytes)") —
	// the room wires it to a UI notice and a transcript line.
	ToolHook func(name, detail string)
	// AskApproval, when set, is consulted before every run_command:
	// the host decides whether the shell may run (the terminal prompts
	// the user; the daemon holds the ask for the room's answer — the
	// same human-in-the-loop a pipeline's `approval: ask` gives). When
	// nil — no approver attached — shell commands are denied
	// deterministically: a room with no human path never runs one.
	AskApproval func(ctx context.Context, tool, detail string) (approved bool, reason string, err error)
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
	// DeepDive says answering well needs codebase facts the digest and
	// area notes likely don't cover — a candidate for the executor-backed
	// deep dive. Only meaningful for personas with tools and a host that
	// can launch dives; consumers must check both before offering.
	DeepDive bool   `json:"deep_dive"`
	Topic    string `json:"dive_topic"` // specific: "session invalidation in auth/"
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
		"speak":      map[string]any{"type": "boolean"},
		"priority":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
		"reason":     map[string]any{"type": "string"},
		"deep_dive":  map[string]any{"type": "boolean"},
		"dive_topic": map[string]any{"type": "string"},
	},
	"required":             []string{"speak", "priority", "reason", "deep_dive", "dive_topic"},
	"additionalProperties": false,
}

// diveSchema is DecideDive's contract: one structured call deciding
// whether a tagged tool-using agent's reply warrants offering a deep
// dive. Tagged agents reply unconditionally, so there is no speak
// dimension here — only the dive question.
var diveSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"deep_dive":  map[string]any{"type": "boolean"},
		"dive_topic": map[string]any{"type": "string"},
		"reason":     map[string]any{"type": "string"},
	},
	"required":             []string{"deep_dive", "dive_topic", "reason"},
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

// Base exposes the persona's composed identity preface (persona system
// prompt, workspace brief, knowledge digest, room framing) so hosts can
// send the exact same grounding on an executor-backed deep dive.
func (a *Agent) Base(room []config.Persona) string { return a.base(room) }

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

// replyMaxTokens caps the plain (tool-less) reply: the contract asks
// for a few sentences, this is the physical backstop. Tool-loop rounds
// stay uncapped on purpose — a write_file's arguments are output
// tokens too, and a clipped tool call breaks the loop, not just prose.
const replyMaxTokens = 400

// replyContract is the house style for room replies, appended to every
// persona's base prompt on both reply paths. Chat brevity is machinery,
// not persona: left to their defaults, models write report-shaped
// essays nobody in a chat actually reads. The pressure valve is the
// workspace — long-form belongs in a file, the room gets one line.
const replyContract = `
Reply to the user directly. Stay strictly in your role's perspective.
Never prefix your reply with your own name.

This is a chat message, not a report: it is read in one glance or
skipped. Two or three sentences, about 80 words as the ceiling; no
headings, no tier lists, and bullets only when you are genuinely
listing items. Lead with the answer or verdict, give the one reason
that matters, stop. If a reply honestly needs more — a plan, a full
review, a spec — write it to a file and post one line with the path
and the gist. Never repeat what another participant already said.`

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
	system := strings.TrimSpace(a.base(room) + replyContract)
	req := llm.Request{
		Model:     a.model(),
		System:    system,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: conversation}},
		MaxTokens: replyMaxTokens,
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
Workspace orientation: everything loop keeps lives under .loop/ —
pipeline and room YAML in .loop/pipelines/ and .loop/rooms/, this
project's persona library in .loop/personas/, approved plans in
.loop/plans/. Pipeline runs keep their records under
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
	system := strings.TrimSpace(a.base(room) + replyContract + `

You have tools: read_file, write_file, run_command — confined to the
workspace. The room is for planning, and the machinery enforces it:
write_file only accepts plans under .loop/plans/, and run_command is
for validation (tests, builds, running the program) — it refuses file
writes, redirection, and inline scripts, so never reach for them. When
the user asks for a change, discuss the approach and its trade-offs,
then offer to write the plan or run the implement pipeline; do not
implement it here. Keep tool use purposeful; conversation is your main
job.` + workspaceOrientation)

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
			out, terr := a.execTool(ctx, tc)
			a.reportTool(tc, out, terr)
			// Tool errors go back to the model as results, not as fatal
			// failures: it can correct a bad path and retry.
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: outOrError(out, terr)})
		}
	}
	return "", fmt.Errorf("%s exceeded %d tool rounds without answering — narrow the request", a.Persona.Name, MaxToolRounds)
}

// execTool runs one tool call, holding shell commands for the host's
// approval first (see AskApproval). A denial goes back to the model as
// tool content — it can answer without the command or propose another.
func (a *Agent) execTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	if tc.Name == "run_command" {
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(tc.Args), &args)
		approved, reason, err := a.approve(ctx, strings.TrimSpace(args.Command))
		if err != nil {
			return "", fmt.Errorf("approval: %w", err)
		}
		if !approved {
			if reason == "" {
				reason = "the user did not approve this command"
			}
			return "denied: " + reason + " — answer without running it, or propose a different approach.", nil
		}
	}
	return execRoomTool(ctx, tc.Name, tc.Args, a.CWD)
}

// approve consults the host's approver; with none attached, shell
// commands are denied — never silently run.
func (a *Agent) approve(ctx context.Context, cmd string) (bool, string, error) {
	if a.AskApproval == nil {
		return false, "no approver is attached to this room — shell commands are denied", nil
	}
	return a.AskApproval(ctx, "run_command", cmd)
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
		detail = summarizeCommand(cmd)
		if strings.HasPrefix(out, "denied:") {
			detail += " — denied"
		}
	default:
		detail = tc.Name
	}
	if err != nil {
		detail += fmt.Sprintf(" — failed: %v", err)
	}
	a.ToolHook(tc.Name, strings.TrimSpace(detail))
}

// summarizeCommand renders a shell command as one short transcript
// line: the first line, clipped, with a note when the command spans
// more. The room is a conversation — a full heredoc blob is noise to
// read, and the operator can ask the agent what it ran.
func summarizeCommand(cmd string) string {
	lines := strings.Split(strings.TrimSpace(cmd), "\n")
	first := strings.TrimSpace(lines[0])
	const maxCommandChars = 96
	if len(first) > maxCommandChars {
		first = first[:maxCommandChars] + "…"
	}
	if len(lines) > 1 {
		first += fmt.Sprintf(" … (+%d lines)", len(lines)-1)
	}
	return first
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
3 — marginal color only. 1-2 — the silent cases above.

deep_dive: you have tools for real investigation (searching the
codebase, reading files, running checks). true only when answering
well needs facts from the codebase or workspace that the conversation,
your brief, and the project notes likely do NOT already cover — the
kind of answer where you would otherwise be guessing. false when your
role's judgment, the conversation, or the notes already carry it.
dive_topic: when deep_dive is true, name exactly what to verify,
specific enough to start from ("session invalidation logic in
auth/", "where env vars are read in config/"). Empty string when
deep_dive is false.`,
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

// DecideDive asks whether this tagged tool-using agent's reply warrants
// offering a deep dive. Tagged agents always speak, so the only question
// is the dive; one cheap structured call on the agent's own (metered)
// provider. The room only consults it when dives are launchable.
func (a *Agent) DecideDive(ctx context.Context, conversation string) (Decision, error) {
	role := a.Persona.Role
	if role == "" {
		role = "colleague"
	}
	req := llm.Request{
		Model:  a.model(),
		System: fmt.Sprintf("You decide whether %s's answer to the CEO's latest message would be materially better after real investigation of the workspace (searching code, reading files, running checks). You are not deciding whether to answer — %s always answers.", a.Persona.Name, a.Persona.Name),
		Messages: []llm.Message{{Role: llm.RoleUser, Content: fmt.Sprintf(`The conversation so far — the CEO's new message is its LAST line:

%s

You are %s, the %s. You will reply from your seat with what you know
(your brief, the conversation, the project notes). Answer two things:

deep_dive: would your answer be materially better — less guessing,
more verified fact — if you first investigated the workspace? true
only when the missing facts are things the codebase or files would
tell you; false when judgment and what's already in context carry it.
dive_topic: when true, exactly what to verify, specific enough to
start from ("session invalidation logic in auth/"). Empty when false.`,
			conversation, a.Persona.Name, role)}},
		ResponseSchema: diveSchema,
		MaxTokens:      300,
	}
	resp, err := a.Provider.Complete(ctx, req)
	if err != nil {
		return Decision{}, err
	}
	var d Decision
	if err := json.Unmarshal([]byte(resp.Text), &d); err != nil {
		return Decision{}, fmt.Errorf("dive decision was not valid JSON (%w); raw: %.120s", err, resp.Text)
	}
	return d, nil
}

func (a *Agent) model() string {
	// Nil-safe: a persona without a model block is env-driven.
	return a.Persona.Model.Resolve().Model
}
