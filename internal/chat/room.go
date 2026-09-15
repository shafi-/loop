package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
)

// Defaults when the room YAML leaves settings unset.
const (
	DefaultSpeakThreshold        = 0.6
	DefaultMaxSpontaneousReplies = 2
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
	known := map[string]bool{}
	byName := map[string]*agent.Agent{}
	for _, a := range r.Agents {
		known[a.Name()] = true
		byName[a.Name()] = a
	}
	if err := r.Transcript.Append("user", text); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}

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
	return nil
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
func (r *Room) reply(ctx context.Context, a *agent.Agent, ui UI) error {
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

// lastUser returns the most recent user message text.
func lastUser(t *Transcript) string {
	for i := len(t.Messages) - 1; i >= 0; i-- {
		if t.Messages[i].From == "user" {
			return t.Messages[i].Text
		}
	}
	return ""
}
