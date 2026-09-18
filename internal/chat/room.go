package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// Defaults when the room YAML leaves settings unset.
const (
	DefaultSpeakThreshold = 0.6
	// The user is the CEO: the point of a room is several perspectives
	// per turn, so the cap guards true pile-ons rather than rationing
	// answers. Rooms can still tune it via max_spontaneous_replies.
	DefaultMaxSpontaneousReplies = 4
	DefaultHistoryWindow         = 50
)

// UI receives everything the terminal should show. The room is UI-agnostic;
// the CLI wires a terminal implementation, tests wire recorders.
type UI interface {
	// AgentReplyStart is emitted before an agent's reply streams.
	AgentReplyStart(name string)
	// AgentTextDelta receives streamed reply text.
	AgentTextDelta(name, delta string)
	// AgentsSeen reports observers who read the turn and chose silence —
	// rendered like a read receipt (👁 seen by …).
	AgentsSeen(names []string)
	// AgentCapped reports an observer who wanted to speak but was held
	// back by the anti-pile-on cap.
	AgentCapped(name string, priority int)
	// Notice reports non-fatal events (observer decision failures).
	Notice(format string, args ...any)
}

// CustomCommand is a room verb beyond chatting ("/name args…"): parsed
// once in Room.Say, so every client that talks to the room — local CLI,
// attached CLI, web composer via the daemon — behaves identically.
// Guard, when set, can veto the command before Handle runs; both a veto
// and a Handle error land in the transcript as a system line, the one
// surface every client shares.
type CustomCommand struct {
	Name   string                                              // the verb, without the slash
	Guard  func(args []string) error                           // optional veto
	Handle func(ctx context.Context, args []string, ui UI) error
}

// Room is one configured multi-agent channel.
type Room struct {
	Name     string
	Personas []config.Persona
	Agents   []*agent.Agent
	Settings struct {
		SpeakThreshold        float64
		MaxSpontaneousReplies int
		HistoryWindow         int
		// MaxContextBytes bounds the rendered conversation string sent
		// to models (0 = DefaultMaxContextBytes).
		MaxContextBytes int
		// KnowledgeDisabled records settings.knowledge: false.
		KnowledgeDisabled bool
	}
	Transcript *Transcript
	// Workspace is the room's workspace root (the directory the room
	// was opened in). /notes reads the knowledge layer through it.
	Workspace string
	// Knowledge maintains the workspace's project knowledge layer
	// (.loop/knowledge/). When set, every turn that wrote files ends
	// with an incremental refresh, announced as a system line. Hosts
	// attach it via SetKnowledge; nil = disabled.
	Knowledge *knowledge.Manager
	// Usage is the session's token ledger; hosts set it when they wrap
	// the agents' providers. The /cost command and the hosting surface
	// read it. nil = this room's cost is not tracked.
	Usage *usage.Meter
	// wrote counts write_file calls this turn — the "implementation
	// happened" signal for post-turn knowledge maintenance.
	wrote int
	// CustomCommands are the room's verbs; NewRoom seeds the built-in
	// rotation commands (/reset, /fork <n>) and /cost, and hosts may
	// adjust their guards or add their own via Command.
	CustomCommands []CustomCommand
}

// NewRoom wires agents against their personas and applies settings
// defaults (0 = unset in the YAML).
func NewRoom(cfg config.Room, agents []*agent.Agent, t *Transcript) *Room {
	r := &Room{Name: cfg.Name, Personas: cfg.Agents, Agents: agents, Transcript: t}
	r.Settings.SpeakThreshold = cfg.Settings.SpeakThreshold
	if r.Settings.SpeakThreshold <= 0 {
		r.Settings.SpeakThreshold = DefaultSpeakThreshold
	}
	r.Settings.MaxSpontaneousReplies = cfg.Settings.MaxSpontaneousReplies
	if r.Settings.MaxSpontaneousReplies <= 0 {
		r.Settings.MaxSpontaneousReplies = DefaultMaxSpontaneousReplies
	}
	r.Settings.HistoryWindow = cfg.Settings.HistoryWindow
	if r.Settings.HistoryWindow <= 0 {
		r.Settings.HistoryWindow = DefaultHistoryWindow
	}
	r.Settings.MaxContextBytes = cfg.Settings.MaxContextBytes
	if r.Settings.MaxContextBytes <= 0 {
		r.Settings.MaxContextBytes = DefaultMaxContextBytes
	}
	r.Settings.KnowledgeDisabled = cfg.Settings.Knowledge != nil && !*cfg.Settings.Knowledge
	r.CustomCommands = append(r.rotationCommands(), r.costCommand(), r.compactCommand(), r.notesCommand())
	return r
}

// SetKnowledge attaches the knowledge layer; a room whose settings say
// knowledge: false declines it. Hosts call this once, after NewRoom.
func (r *Room) SetKnowledge(m *knowledge.Manager) {
	if r.Settings.KnowledgeDisabled {
		return
	}
	r.Knowledge = m
}

// notesCommand seeds /notes: the knowledge layer for zero tokens —
// list the area notes, or print one. Deterministic file reads appended
// as system lines, so every attached client and every non-tool agent
// sees the same thing.
func (r *Room) notesCommand() CustomCommand {
	return CustomCommand{
		Name: "notes",
		Handle: func(_ context.Context, args []string, ui UI) error {
			line := knowledge.NotesReport(r.Workspace, args)
			if err := r.Transcript.Append("system", line); err != nil {
				return err
			}
			ui.Notice("%s", line)
			return nil
		},
	}
}

// costCommand seeds /cost: the session's token ledger as one system
// line, so every attached client — terminal, web, daemon — sees the
// same answer without any surface-specific plumbing.
func (r *Room) costCommand() CustomCommand {
	return CustomCommand{
		Name: "cost",
		Handle: func(_ context.Context, args []string, ui UI) error {
			line := r.CostReport()
			if err := r.Transcript.Append("system", line); err != nil {
				return err
			}
			ui.Notice("%s", line)
			return nil
		},
	}
}

// maxCompactInput bounds the text one /compact summarizes; the tail is
// what matters (older turns are the first to fall off anyway).
const maxCompactInput = 32 << 10

// compactCommand seeds /compact: one LLM call summarizes everything
// before the live window into a "session so far" line; the summarized
// prefix archives (never destroyed) and the summary rides as the first
// context line. Manual by design — compaction spends the user's tokens
// only when they ask.
func (r *Room) compactCommand() CustomCommand {
	return CustomCommand{
		Name: "compact",
		Handle: func(ctx context.Context, _ []string, ui UI) error {
			window := r.Settings.HistoryWindow
			msgs := r.Transcript.Window(0)
			if len(msgs) <= window {
				return r.Transcript.Append("system",
					"◈ nothing to compact — the whole session still fits in the live window")
			}
			if len(r.Agents) == 0 {
				return r.Transcript.Append("system", "◈ /compact needs at least one agent")
			}
			prefix := Render(msgs[:len(msgs)-window])
			if len(prefix) > maxCompactInput {
				prefix = "[… older turns clipped …]\n" + prefix[len(prefix)-maxCompactInput:]
			}
			provider := r.Agents[0].Provider
			if r.Usage != nil {
				provider = usage.Wrap(provider, r.Usage, "compact")
			}
			resp, err := provider.Complete(ctx, llm.Request{
				Model: r.Agents[0].Model(),
				System: `You summarize a strategy-room session for its participants.
One tight paragraph: what was decided, what is open, facts established,
and what remains. No preamble, no bullet-point ceremony.`,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: prefix + "\n\nSummarize this session so far."}},
				MaxTokens: 700,
			})
			if err != nil {
				return r.Transcript.Append("system", "◈ /compact failed: "+err.Error())
			}
			summary := strings.TrimSpace(resp.Text)
			if summary == "" {
				return r.Transcript.Append("system", "◈ /compact produced no summary — transcript unchanged")
			}
			archive := fmt.Sprintf("transcript-%s.jsonl", time.Now().UTC().Format("20060102-150405"))
			if err := r.Transcript.Compact(archive, window, "◈ session so far: "+summary); err != nil {
				return err
			}
			ui.Notice("◈ compacted — %d turns summarized, archived as %s", len(msgs)-window, archive)
			return nil
		},
	}
}

// CostReport renders the session's usage: the total plus per-agent
// attribution when the ledger knows more than one contributor.
func (r *Room) CostReport() string {
	if r.Usage == nil {
		return "◈ usage: not tracked for this session"
	}
	tot := r.Usage.Totals()
	if tot.Calls == 0 {
		return "◈ usage: no model calls recorded yet"
	}
	report := fmt.Sprintf("◈ session usage — %s", tot.FormatTotal())
	if entries := r.Usage.Snapshot(); len(entries) > 1 {
		var parts []string
		for _, e := range entries {
			parts = append(parts, fmt.Sprintf("%s: %d calls · in %s · out %s",
				e.Label, e.Calls, usage.Human(e.InputTokens), usage.Human(e.OutputTokens)))
		}
		report += "\n" + strings.Join(parts, "\n")
	}
	return report
}

// rotationCommands seeds the engine's built-in room verbs.
func (r *Room) rotationCommands() []CustomCommand {
	return []CustomCommand{
		{
			Name: "reset",
			Handle: func(_ context.Context, args []string, ui UI) error {
				archive, err := r.Rotate(0, "reset")
				if err != nil {
					return err
				}
				ui.Notice("↻ reset — archived as %s", archive)
				return nil
			},
		},
		{
			Name: "fork",
			Handle: func(_ context.Context, args []string, ui UI) error {
				if len(args) != 1 {
					return fmt.Errorf("usage: /fork <n> — keep the first n transcript lines")
				}
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 0 {
					return fmt.Errorf("usage: /fork <n> — keep the first n transcript lines")
				}
				archive, err := r.Rotate(n, "fork")
				if err != nil {
					return err
				}
				ui.Notice("↻ fork — archived as %s", archive)
				return nil
			},
		},
	}
}

// Command returns the room verb by name, if registered. Callers may
// adjust its Guard (the daemon vetoes rotation while its pipeline runs
// are alive) — the pointer points into the room's registry.
func (r *Room) Command(name string) *CustomCommand {
	for i := range r.CustomCommands {
		if r.CustomCommands[i].Name == name {
			return &r.CustomCommands[i]
		}
	}
	return nil
}

// Rotate restarts the room's conversation: the current transcript is
// archived, the first keep messages survive (0 = a fresh start), and a
// system line marks the point. kind is "reset" or "fork"; it only
// shapes the note text. Returns the archive file's name.
func (r *Room) Rotate(keep int, kind string) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	archive := fmt.Sprintf("transcript-%s.jsonl", stamp)
	if r.Transcript.path != "" {
		dir := filepath.Dir(r.Transcript.path)
		for i := 2; ; i++ {
			if _, err := os.Stat(filepath.Join(dir, archive)); os.IsNotExist(err) {
				break
			}
			archive = fmt.Sprintf("transcript-%s-%d.jsonl", stamp, i)
		}
	}
	note := fmt.Sprintf("↻ forked here — the discussion continues from this point; everything after is archived as %s", archive)
	if kind == "reset" {
		note = fmt.Sprintf("↻ room reset — the previous discussion is archived as %s", archive)
	}
	return archive, r.Transcript.Rotate(archive, keep, note)
}

// tagged parses @mentions and returns the mentioned agent names in first
// mention order, deduplicated, restricted to known agents.
func tagged(text string, known map[string]bool) []string {
	var order []string
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '@')
	}) {
		if !strings.HasPrefix(word, "@") {
			continue
		}
		name := strings.TrimPrefix(word, "@")
		if known[name] && !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	return order
}

// EnsureKnowledge scans the knowledge layer and refreshes it when stale
// or missing — session-open maintenance. External edits and first seeds
// are caught here; a fresh layer costs zero model calls and stays
// silent. Returns the announcement line, if one was recorded. Safe to
// call from a host goroutine.
func (r *Room) EnsureKnowledge(ctx context.Context) string {
	if r.Knowledge == nil {
		return ""
	}
	line := r.Knowledge.MaintainLine(ctx)
	if line != "" {
		_ = r.Transcript.Append("system", line)
	}
	return line
}

// maintainKnowledge runs after a turn that wrote files: the briefs
// catch up with the implementation that just happened. Never fails the
// turn — a refresh error is a system line, the one shared surface.
func (r *Room) maintainKnowledge(ctx context.Context) {
	if r.Knowledge == nil || r.wrote == 0 {
		return
	}
	r.wrote = 0
	if line := r.Knowledge.MaintainLine(ctx); line != "" {
		_ = r.Transcript.Append("system", line)
	}
}

// Say processes one user message. Turn order matters: tagged agents reply
// FIRST, and only then do observers decide — an observer judging the turn
// before the named agents have spoken would evaluate a conversation that
// is still in motion, and the tagged replies often settle the question.
// With no tags, decisions run immediately.
func (r *Room) Say(ctx context.Context, text string, ui UI) error {
	// Room verbs parse once here, in the engine — the one interface
	// every client drives (local CLI, attached CLI, web composer via
	// the daemon) — so a command means the same thing everywhere. A
	// slash word the room doesn't know is still chat: clients may know
	// verbs this room's engine does not.
	if strings.HasPrefix(text, "/") {
		if verb, args := splitCommand(text); verb != "" {
			if cmd := r.Command(verb); cmd != nil {
				return r.runCommand(ctx, *cmd, args, ui)
			}
		}
	}
	known := map[string]bool{}
	byName := map[string]*agent.Agent{}
	for _, a := range r.Agents {
		known[a.Name()] = true
		byName[a.Name()] = a
	}
	r.wrote = 0
	if err := r.Transcript.Append("user", text); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	base := len(r.Transcript.Messages) // for the produced-nothing check

	// Phase 1: tagged agents reply, in mention order.
	mentions := tagged(text, known)
	for _, name := range mentions {
		if err := r.reply(ctx, byName[name], ui); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	// Phase 2: every other agent decides concurrently — now seeing the
	// tagged replies as part of the conversation.
	observers := r.observers(mentions)
	decisions := r.decideAll(ctx, observers, ui)

	// Phase 3: spontaneous replies — qualify by threshold, order by
	// priority, cap by the anti-pile-on rule. Everyone else gets a
	// read receipt.
	type candidate struct {
		a *agent.Agent
		d agent.Decision
	}
	var qualified []candidate
	var seen []string
	for _, a := range observers {
		d := decisions[a.Name()]
		if d.Speak && d.Confidence() >= r.Settings.SpeakThreshold {
			qualified = append(qualified, candidate{a, d})
		} else {
			seen = append(seen, a.Name())
		}
	}
	sort.Slice(qualified, func(i, j int) bool { return qualified[i].d.Priority > qualified[j].d.Priority })
	if max := r.Settings.MaxSpontaneousReplies; len(qualified) > max {
		for _, q := range qualified[max:] {
			ui.AgentCapped(q.a.Name(), q.d.Priority)
		}
		qualified = qualified[:max]
	}
	for _, q := range qualified {
		if err := r.reply(ctx, q.a, ui); err != nil {
			return fmt.Errorf("%s: %w", q.a.Name(), err)
		}
	}
	if len(seen) > 0 {
		ui.AgentsSeen(seen)
	}

	// A turn that produced nothing (mistyped @name, everyone chose
	// silence) must not read as the room ignoring the user. The engine
	// knows exactly whether any agent authored a line this turn, so the
	// hint lives here — transcript line, plus a local echo.
	if len(r.Transcript.Messages) == base {
		hint := "no agent replied — address @" + r.agentNames() + " to force an answer"
		_ = r.Transcript.Append("system", hint)
		ui.Notice("%s", hint)
	}
	r.maintainKnowledge(ctx)
	return nil
}

// agentNames renders the participants as an @-list for hint lines.
func (r *Room) agentNames() string {
	names := make([]string, 0, len(r.Agents))
	for _, a := range r.Agents {
		names = append(names, a.Name())
	}
	return strings.Join(names, ", @")
}

// observers returns agents not mentioned in the message.
func (r *Room) observers(mentions []string) []*agent.Agent {
	skip := map[string]bool{}
	for _, m := range mentions {
		skip[m] = true
	}
	var out []*agent.Agent
	for _, a := range r.Agents {
		if !skip[a.Name()] {
			out = append(out, a)
		}
	}
	return out
}

// decideAll runs speak-decisions concurrently. An observer whose decision
// call fails stays silent with a notice — one agent's provider hiccup
// must not kill the room.
func (r *Room) decideAll(ctx context.Context, observers []*agent.Agent, ui UI) map[string]agent.Decision {
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := map[string]agent.Decision{}
	// The decision is a cheap structured call — it needs recency, not
	// depth: a tighter window and byte budget than replies. The new
	// message is the conversation's last line (appended before this
	// runs), and the prompt points at it instead of re-embedding it.
	window := r.Settings.HistoryWindow
	if window > DecisionWindowMessages {
		window = DecisionWindowMessages
	}
	conversation := r.Transcript.BuildConversation(window, DecisionContextBytes)
	for _, a := range observers {
		wg.Add(1)
		go func(a *agent.Agent) {
			defer wg.Done()
			d, err := a.DecideSpeak(ctx, r.Personas, conversation)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out[a.Name()] = agent.Decision{Speak: false, Priority: 1, Reason: "decision failed: " + err.Error()}
				ui.Notice("· %s decision call failed (%v) — staying silent", a.Name(), err)
				return
			}
			out[a.Name()] = d
		}(a)
	}
	wg.Wait()
	return out
}

// reply streams one agent's answer and appends it to the transcript.
// Agents with tools get their tool activity surfaced as a notice and
// recorded in the transcript — the room's audit trail, and context for
// the other agents (they learn a file now exists).
func (r *Room) reply(ctx context.Context, a *agent.Agent, ui UI) error {
	a.ToolHook = func(name, detail string) {
		if name == "write_file" {
			r.wrote++
		}
		ui.Notice("🔧 @%s %s", a.Name(), detail)
		_ = r.Transcript.Append(a.Name(), "[tool] "+detail)
	}
	ui.AgentReplyStart(a.Name())
	text, err := a.Reply(ctx, r.Personas, r.Transcript.BuildConversation(r.Settings.HistoryWindow, r.Settings.MaxContextBytes), func(delta string) {
		ui.AgentTextDelta(a.Name(), delta)
	})
	if err != nil {
		return err
	}
	if err := r.Transcript.Append(a.Name(), text); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	return nil
}

// splitCommand splits "/verb args…" into the verb and its arguments.
func splitCommand(text string) (string, []string) {
	fields := strings.Fields(strings.TrimPrefix(text, "/"))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}

// runCommand executes a room verb: the guard may veto, and any failure
// — veto, usage, execution — lands in the transcript as a system line,
// the one surface every client shares. A mistyped command must not read
// as a failed turn.
func (r *Room) runCommand(ctx context.Context, cmd CustomCommand, args []string, ui UI) error {
	if cmd.Guard != nil {
		if err := cmd.Guard(args); err != nil {
			return r.Transcript.Append("system", "↻ "+err.Error())
		}
	}
	if err := cmd.Handle(ctx, args, ui); err != nil {
		return r.Transcript.Append("system", "↻ "+err.Error())
	}
	return nil
}

// lastUser returns the most recent user message text.
func lastUser(t *Transcript) string {
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if t.Messages[i].From == "user" {
			return t.Messages[i].Text
		}
	}
	return ""
}
