package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/knowledge"
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
// surface every client shares. Desc is one line for command menus
// (the web composer's "/" palette, the terminal's "/" listing).

// CustomCommand is a room verb beyond chatting ("/name args…"): parsed
// once in Room.Say, so every client that talks to the room — local CLI,
// attached CLI, web composer via the daemon — behaves identically.
// Guard, when set, can veto the command before Handle runs; both a veto
// and a Handle error land in the transcript as a system line, the one
// surface every client shares. Desc is one line for command menus
// (the web composer's "/" palette, the terminal's "/" listing).
type CustomCommand struct {
	Name   string                    // the verb, without the slash
	Desc   string                    // one line, for menus
	Guard  func(args []string) error // optional veto
	Handle func(ctx context.Context, args []string, ui UI) error
}

// CommandInfo is one entry of the room's verb menu.

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
	// (.loop/knowledge/). The layer only describes landed work: hosts
	// call EnsureKnowledge at session open to reconcile it with the
	// workspace, and completed pipeline runs refresh it in their own
	// process. Agent file writes mid-conversation never touch it —
	// until a pipeline lands, everyone sees the last completed state.
	// Hosts attach it via SetKnowledge; nil = disabled.
	Knowledge *knowledge.Manager
	// Usage is the session's token ledger; hosts set it when they wrap
	// the agents' providers. The /cost command and the hosting surface
	// read it. nil = this room's cost is not tracked.
	Usage *usage.Meter
	// RunUsage reads one pipeline run's token spend from the
	// workspace's run records — the on-disk layout is the engine's, so
	// the room only asks for a number. /cost's pipeline line reads
	// through it; hosts set it at assembly. nil = pipeline spend
	// unreported.
	RunUsage func(runsDir, id string) (usage.Total, bool)
	// Summarizer condenses a transcript prefix into a "session so
	// far" summary — /compact's one model call. Hosts bind it to the
	// room's lead agent (metered under its own /cost label); nil =
	// /compact declines.
	Summarizer func(ctx context.Context, text string) (string, error)
	// StartDive launches an executor-backed deep-dive run for one
	// persona (roomkit builds the one-agent-stage pipeline and starts
	// it like /run: gates ask in the chat, the report lands as a
	// transcript line). Hosts set it at assembly; nil = this room
	// cannot dive, so agents never offer and /deepdive declines.
	StartDive func(ctx context.Context, spec DiveSpec) error
	// DiveUnavailableReason explains why StartDive is nil when the host
	// knows the cause (executor missing vs no run session) — /deepdive's
	// error says exactly that instead of a generic attach hint.
	DiveUnavailableReason string
	// ToolApproval is asked before a room agent's shell command runs —
	// the same human-in-the-loop a pipeline's `approval: ask` gives.
	// The terminal prompts the user; the daemon holds the ask for the
	// room's answer through the gate card / approve endpoint. nil = no
	// approver attached: commands are denied deterministically, never
	// silently run.
	ToolApproval func(ctx context.Context, agent, tool, detail string) (approved bool, reason string, err error)
	// dives is the last-offered-topic memory (deepdive.json beside the
	// transcript); divesLoaded guards its lazy read.
	dives       map[string]divePending
	divesLoaded bool
	// usageBase is every PAST session's spend, loaded from the room's
	// persisted usage.json at open — /cost's "all sessions" line adds
	// it to the live meter, and FlushUsage persists the sum for the
	// next session.
	usageBase usage.Total
	// CustomCommands are the room's verbs; NewRoom seeds the built-in
	// rotation commands (/reset, /fork <n>) and /cost, and hosts may
	// adjust their guards or add their own via Command.
	CustomCommands []CustomCommand
}

// NewRoom wires agents against their personas and applies settings
// defaults (0 = unset in the YAML).

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
	r.CustomCommands = append(r.rotationCommands(), r.costCommand(), r.compactCommand(), r.notesCommand(), r.deepDiveCommand())
	r.usageBase = loadUsageBase(t.Dir())
	return r
}

// usageFile is the room's lifetime spend of past sessions, one JSON
// usage.Total, next to the transcript.

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
// or missing — session-open maintenance, the reconciliation point with
// the workspace: external edits since last session, writes from
// completed pipeline runs. A fresh layer costs zero model calls and
// stays silent. Returns the announcement line, if one was recorded.
// Safe to call from a host goroutine.

// EnsureKnowledge scans the knowledge layer and refreshes it when stale
// or missing — session-open maintenance, the reconciliation point with
// the workspace: external edits since last session, writes from
// completed pipeline runs. A fresh layer costs zero model calls and
// stays silent. Returns the announcement line, if one was recorded.
// Safe to call from a host goroutine.
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

// Say processes one user message. Turn order matters: tagged agents reply
// FIRST, and only then do observers decide — an observer judging the turn
// before the named agents have spoken would evaluate a conversation that
// is still in motion, and the tagged replies often settle the question.
// With no tags, decisions run immediately.

// Say processes one user message. Turn order matters: tagged agents reply
// FIRST, and only then do observers decide — an observer judging the turn
// before the named agents have spoken would evaluate a conversation that
// is still in motion, and the tagged replies often settle the question.
// With no tags, decisions run immediately.
func (r *Room) Say(ctx context.Context, text string, ui UI) error {
	// Whatever this turn spends (replies, decisions, /compact) lands in
	// the room's lifetime ledger on the way out.
	defer r.FlushUsage()
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
	if err := r.Transcript.Append("user", text); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	base := len(r.Transcript.Messages) // for the produced-nothing check

	// Phase 1: tagged agents reply, in mention order. A tool-using
	// agent first decides (one cheap structured call) whether its
	// answer would be materially better after real investigation —
	// if so, the room appends a deep-dive offer line right after its
	// reply (its CTA is the /deepdive command).
	mentions := tagged(text, known)
	for _, name := range mentions {
		a := byName[name]
		var dive agent.Decision
		if r.canDive(a) {
			conversation := r.Transcript.BuildConversation(r.Settings.HistoryWindow, r.Settings.MaxContextBytes)
			if d, err := a.DecideDive(ctx, conversation); err == nil {
				dive = d
			} else {
				ui.Notice("· %s dive decision failed (%v) — replying without an offer", name, err)
			}
		}
		if err := r.reply(ctx, a, ui); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if dive.DeepDive && strings.TrimSpace(dive.Topic) != "" {
			r.offerDive(a, dive.Topic, text, ui)
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
		if q.d.DeepDive && strings.TrimSpace(q.d.Topic) != "" && r.canDive(q.a) {
			r.offerDive(q.a, q.d.Topic, text, ui)
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
	return nil
}

// agentNames renders the participants as an @-list for hint lines.

// agentNames renders the participants as an @-list for hint lines.
func (r *Room) agentNames() string {
	names := make([]string, 0, len(r.Agents))
	for _, a := range r.Agents {
		names = append(names, a.Name())
	}
	return strings.Join(names, ", @")
}

// observers returns agents not mentioned in the message.

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

// reply streams one agent's answer and appends it to the transcript.
// Agents with tools get their tool activity surfaced as a notice and
// recorded in the transcript — the room's audit trail, and context for
// the other agents (they learn a file now exists).
func (r *Room) reply(ctx context.Context, a *agent.Agent, ui UI) error {
	a.ToolHook = func(name, detail string) {
		ui.Notice("🔧 @%s %s", a.Name(), detail)
		_ = r.Transcript.Append(a.Name(), "[tool] "+detail)
	}
	a.AskApproval = func(ctx context.Context, tool, detail string) (bool, string, error) {
		if r.ToolApproval == nil {
			return false, "no approver is attached to this room — shell commands are denied", nil
		}
		return r.ToolApproval(ctx, a.Name(), tool, detail)
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

// lastUser returns the most recent user message text.
func lastUser(t *Transcript) string {
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if t.Messages[i].From == "user" {
			return t.Messages[i].Text
		}
	}
	return ""
}
