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

// Room is one configured multi-agent channel.
type Room struct {
	Name     string
	Personas []config.Persona
	Agents   []*agent.Agent
	Settings struct {
		SpeakThreshold        float64
		MaxSpontaneousReplies int
		HistoryWindow         int
	}
	Transcript *Transcript
	// RotateGuard, when set, can veto /reset and /fork (the daemon
	// refuses while its pipeline runs are alive or waiting).
	RotateGuard func() error
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
	return r
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

// Say processes one user message. Turn order matters: tagged agents reply
// FIRST, and only then do observers decide — an observer judging the turn
// before the named agents have spoken would evaluate a conversation that
// is still in motion, and the tagged replies often settle the question.
// With no tags, decisions run immediately.
func (r *Room) Say(ctx context.Context, text string, ui UI) error {
	// The rotation verbs parse once here, in the room engine — the one
	// interface every client drives (local CLI, attached CLI, web
	// composer via the daemon) — so /reset and /fork behave identically
	// everywhere and never become messages the agents puzzle over.
	if text == "/reset" || strings.HasPrefix(text, "/fork") {
		return r.rotateCommand(text, ui)
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
	conversation := r.Transcript.BuildConversation(r.Settings.HistoryWindow)
	newMessage := lastUser(r.Transcript)
	for _, a := range observers {
		wg.Add(1)
		go func(a *agent.Agent) {
			defer wg.Done()
			d, err := a.DecideSpeak(ctx, r.Personas, conversation, newMessage)
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
	text, err := a.Reply(ctx, r.Personas, r.Transcript.BuildConversation(r.Settings.HistoryWindow), func(delta string) {
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

// rotateCommand handles the conversation-rotation verbs (/reset,
// /fork <n>). Feedback is a system line in the transcript — the one
// surface every client shares — plus ui.Notice for an immediate local
// echo. Usage mistakes and vetoes are system lines too, never errors:
// a mistyped command must not read as a failed turn.
func (r *Room) rotateCommand(text string, ui UI) error {
	kind, keep := "reset", 0
	if text != "/reset" {
		kind = "fork"
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(text, "/fork")))
		if err != nil || n < 0 {
			return r.Transcript.Append("system", "↻ usage: /fork <n> — keep the first n transcript lines")
		}
		keep = n
	}
	if r.RotateGuard != nil {
		if err := r.RotateGuard(); err != nil {
			return r.Transcript.Append("system", "↻ "+err.Error())
		}
	}
	archive, err := r.Rotate(keep, kind)
	if err != nil {
		return err
	}
	ui.Notice("↻ %s — archived as %s", kind, archive)
	return nil
}

// lastUser returns the most recent user message text.
func lastUser(t *Transcript) string {	for i := len(t.Messages) - 1; i >= 0; i-- {
		if t.Messages[i].From == "user" {
			return t.Messages[i].Text
		}
	}
	return ""
}
