package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
type CustomCommand struct {
	Name   string                    // the verb, without the slash
	Desc   string                    // one line, for menus
	Guard  func(args []string) error // optional veto
	Handle func(ctx context.Context, args []string, ui UI) error
}

// CommandInfo is one entry of the room's verb menu.
type CommandInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// CommandList returns the room's verbs with their one-line descriptions,
// in registration order — what "/" surfaces to the user.
func (r *Room) CommandList() []CommandInfo {
	out := make([]CommandInfo, 0, len(r.CustomCommands))
	for _, c := range r.CustomCommands {
		if c.Handle == nil {
			continue
		}
		out = append(out, CommandInfo{Name: c.Name, Desc: c.Desc})
	}
	return out
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
	r.usageBase = loadUsageBase(t.Dir())
	return r
}

// usageFile is the room's lifetime spend of past sessions, one JSON
// usage.Total, next to the transcript.
func usageFile(dir string) string { return filepath.Join(dir, "usage.json") }

func loadUsageBase(dir string) usage.Total {
	if dir == "" {
		return usage.Total{}
	}
	data, err := os.ReadFile(usageFile(dir))
	if err != nil {
		return usage.Total{} // first session, or unreadable: start at zero
	}
	var t usage.Total
	if json.Unmarshal(data, &t) != nil {
		return usage.Total{} // corrupt: a ledger must never break the room
	}
	return t
}

// FlushUsage persists the room's lifetime spend (past sessions + this
// one so far) as usage.json next to the transcript, atomically. Called
// at the end of every turn; hosts also call it after out-of-turn spend
// (the knowledge layer). Best-effort: a flush failure never fails a turn.
func (r *Room) FlushUsage() {
	if r.Usage == nil {
		return
	}
	dir := r.Transcript.Dir()
	if dir == "" {
		return // in-memory transcript (tests): nothing to persist
	}
	data, err := json.Marshal(r.usageBase.Add(r.Usage.Totals()))
	if err != nil {
		return
	}
	tmp := usageFile(dir) + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, usageFile(dir))
}

// UsageAll is the room's lifetime conversation spend: every past
// session plus this one so far.
func (r *Room) UsageAll() usage.Total {
	if r.Usage == nil {
		return r.usageBase
	}
	return r.usageBase.Add(r.Usage.Totals())
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
		Desc: "project knowledge: list the area notes, or read one (/notes <slug>)",
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
		Desc: "this session's token ledger, per agent when several spoke",
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
		Desc: "one model call: older turns become a \"session so far\" summary",
		Handle: func(ctx context.Context, _ []string, ui UI) error {
			window := r.Settings.HistoryWindow
			msgs := r.Transcript.Window(0)
			if len(msgs) <= window {
				return r.Transcript.Append("system",
					"◈ nothing to compact — the whole session still fits in the live window")
			}
			if r.Summarizer == nil {
				return r.Transcript.Append("system", "◈ /compact needs an agent to summarize with")
			}
			prefix := Render(msgs[:len(msgs)-window])
			if len(prefix) > maxCompactInput {
				prefix = "[… older turns clipped …]\n" + prefix[len(prefix)-maxCompactInput:]
			}
			summary, err := r.Summarizer(ctx, prefix)
			if err != nil {
				return r.Transcript.Append("system", "◈ /compact failed: "+err.Error())
			}
			summary = strings.TrimSpace(summary)
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

// CostReport renders what this room has spent, at three depths: this
// session (with per-agent attribution), every session since the room
// began, and the pipeline runs it commanded (their spend happens in
// child `loop run` processes, so it is read back from their run
// records — any session, current and archived transcripts alike).
func (r *Room) CostReport() string {
	if r.Usage == nil {
		return "◈ usage: not tracked for this session"
	}
	tot := r.Usage.Totals()
	var b strings.Builder
	if tot.Calls == 0 {
		b.WriteString("◈ session usage: no model calls recorded yet")
	} else {
		fmt.Fprintf(&b, "◈ session usage — %s", tot.FormatTotal())
		if entries := r.Usage.Snapshot(); len(entries) > 1 {
			var parts []string
			for _, e := range entries {
				parts = append(parts, fmt.Sprintf("%s: %d calls · in %s · out %s",
					e.Label, e.Calls, usage.Human(e.InputTokens), usage.Human(e.OutputTokens)))
			}
			b.WriteString("\n" + strings.Join(parts, "\n"))
		}
	}
	if r.usageBase.Calls > 0 {
		fmt.Fprintf(&b, "\n◈ all sessions — %s (this room's conversations, since the beginning)",
			r.UsageAll().FormatTotal())
	}
	if pu, runs := r.pipelineUsage(); runs > 0 {
		fmt.Fprintf(&b, "\n◈ pipelines — %s (%d run%s commanded from this room; spend lives in their run records)",
			pu.FormatTotal(), runs, plural(runs))
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// roomRunIDRe matches loop's generated run ids as they appear in
// transcript text (progress lines, completion lines, resume hints) —
// the record of which runs this room commanded.
var roomRunIDRe = regexp.MustCompile(`\b\d{8}-\d{6}-[0-9a-f]{4}\b`)

// pipelineUsage sums the spend of every run this room's transcripts
// mention — current and archived — from .loop/runs/<id>/state.json.
func (r *Room) pipelineUsage() (usage.Total, int) {
	if r.Workspace == "" {
		return usage.Total{}, 0
	}
	seen := map[string]bool{}
	collect := func(text string) {
		for _, id := range roomRunIDRe.FindAllString(text, -1) {
			seen[id] = true
		}
	}
	for _, m := range r.Transcript.Window(0) {
		collect(m.Text)
	}
	if dir := r.Transcript.Dir(); dir != "" {
		if archives, err := filepath.Glob(filepath.Join(dir, "transcript-*.jsonl")); err == nil {
			for _, path := range archives {
				if data, err := os.ReadFile(path); err == nil {
					collect(string(data))
				}
			}
		}
	}
	runsDir := filepath.Join(r.Workspace, ".loop", "runs")
	if r.RunUsage == nil {
		return usage.Total{}, 0
	}
	var tot usage.Total
	n := 0
	for id := range seen {
		if u, ok := r.RunUsage(runsDir, id); ok {
			tot = tot.Add(u)
			n++
		}
	}
	return tot, n
}

// rotationCommands seeds the engine's built-in room verbs.
func (r *Room) rotationCommands() []CustomCommand {
	return []CustomCommand{
		{
			Name: "reset",
			Desc: "archive this conversation and start a fresh one",
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
			Desc: "keep the first n lines, archive the rest (/fork <n>)",
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
