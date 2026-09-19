// The room's deep-dive tier: a tool-using persona's investigation can
// escalate to an executor-backed run (cline). The engine's job here is
// deliberately small: after a reply that would be better verified, emit
// a stable offer line — whose CTA the UIs render as a button — and
// handle the /deepdive command that button (or a typed line) sends.
// There is no consent detection: launching is always an explicit
// command, so the engine never guesses at free text. The launch itself
// is the host's (roomkit builds a one-agent-stage pipeline and starts
// it like /run), injected as Room.StartDive.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shafi-/loop/internal/agent"
)

// DiveSpec is one launchable deep dive — everything the host needs to
// build the run, composed by the room from its own state.
type DiveSpec struct {
	Agent       string   // persona name
	Role        string   // persona role, for the generated pipeline's record
	Topic       string   // what to verify, specific
	Question    string   // the user message that prompted the offer
	Tools       []string // the persona's grants (host filters room-native names)
	System      string   // the persona's composed Base — same grounding as its replies
	ContextTail string   // recent conversation, for continuity
}

// diveOfferMarker prefixes every offer line. It is a contract with the
// UIs: a transcript line starting with this marker renders as an offer
// card with a button that sends the line's /deepdive command. The
// terminal just shows the line — the command is right there to type.
const diveOfferMarker = "◈ deep-dive offer"

// divePending is an agent's last offered dive — topic memory so
// "/deepdive @architect" without a topic reuses it. Purely a
// convenience: offers never gate anything, so they need no expiry.
type divePending struct {
	Topic    string `json:"topic"`
	Question string `json:"question"`
}

// canDive: an agent can be offered a dive only when it has tools to
// investigate with and the host attached a launcher. Offering otherwise
// would be a lie — the room never dangles what it cannot run.
func (r *Room) canDive(a *agent.Agent) bool {
	return r.StartDive != nil && len(a.Persona.Tools) > 0
}

// loadDives reads deepdive.json beside the transcript, tolerant of a
// missing or corrupt file (nothing offered).
func (r *Room) loadDives() map[string]divePending {
	if r.divesLoaded {
		return r.dives
	}
	r.divesLoaded = true
	r.dives = map[string]divePending{}
	if dir := r.Transcript.Dir(); dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, "deepdive.json")); err == nil {
			_ = json.Unmarshal(data, &r.dives) // corrupt = nothing offered
		}
	}
	return r.dives
}

// saveDives persists the offer map atomically, the usage.json pattern.
func (r *Room) saveDives() {
	dir := r.Transcript.Dir()
	if dir == "" {
		return
	}
	data, err := json.Marshal(r.dives)
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, ".deepdive.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, "deepdive.json"))
}

// offerDive records the offered topic and appends the offer line — the
// deterministic contract the CTA renders. Called right after an agent's
// reply when its decision flagged deep_dive.
func (r *Room) offerDive(a *agent.Agent, topic, question string, ui UI) {
	r.dives = r.loadDives()
	r.dives[a.Name()] = divePending{Topic: topic, Question: question}
	r.saveDives()
	line := fmt.Sprintf("%s @%s — topic: %s — run: /deepdive @%s", diveOfferMarker, a.Name(), topic, a.Name())
	_ = r.Transcript.Append("system", line)
	ui.Notice("%s", line)
}

// launchDive composes the spec from room state and hands it to the
// host's launcher. Launch errors land as system lines, never failures
// of the turn.
func (r *Room) launchDive(ctx context.Context, a *agent.Agent, topic, question string, ui UI) {
	spec := DiveSpec{
		Agent:       a.Name(),
		Role:        a.Persona.Role,
		Topic:       topic,
		Question:    question,
		Tools:       a.Persona.Tools,
		System:      a.Base(r.Personas),
		ContextTail: r.Transcript.BuildConversation(r.Settings.HistoryWindow, r.Settings.MaxContextBytes),
	}
	if err := r.StartDive(ctx, spec); err != nil {
		line := "↻ deep dive: " + err.Error()
		_ = r.Transcript.Append("system", line)
		ui.Notice("%s", line)
		return
	}
	line := fmt.Sprintf("▶ @%s deep dive started: %s — its report lands here when the run completes", a.Name(), topic)
	_ = r.Transcript.Append("system", line)
	ui.Notice("%s", line)
}

// deepDiveCommand seeds /deepdive — the one door to a dive, whether
// clicked as a CTA or typed. No pending offer is required: an explicit
// topic launches directly; without one, the agent's last offered topic
// is reused.
func (r *Room) deepDiveCommand() CustomCommand {
	return CustomCommand{
		Name: "deepdive",
		Desc: "launch an executor-backed deep dive (/deepdive @architect [topic])",
		Handle: func(ctx context.Context, args []string, ui UI) error {
			if r.StartDive == nil {
				if r.DiveUnavailableReason != "" {
					return fmt.Errorf("deep dives unavailable: %s", r.DiveUnavailableReason)
				}
				return fmt.Errorf("deep dives need a run session — attach with loop chat or the daemon (executor: loop setup)")
			}
			if len(args) == 0 {
				return fmt.Errorf("usage: /deepdive @agent [topic] — agents that can dive: %s", r.diverNames())
			}
			name := strings.TrimPrefix(args[0], "@")
			a := r.agentByName(name)
			if a == nil {
				return fmt.Errorf("no agent %q in this room", name)
			}
			if !r.canDive(a) {
				return fmt.Errorf("@%s has no tools to deep-dive with (grant tools: in the room YAML)", name)
			}
			topic := strings.Join(args[1:], " ")
			question := lastUser(r.Transcript)
			if topic == "" {
				offered, ok := r.loadDives()[name]
				if !ok {
					return fmt.Errorf("no topic given and @%s has no pending offer — add one: /deepdive @%s <what to verify>", name, name)
				}
				topic, question = offered.Topic, offered.Question
			}
			r.launchDive(ctx, a, topic, question, ui)
			return nil
		},
	}
}

// diverNames lists agents that can dive, for usage errors.
func (r *Room) diverNames() string {
	var names []string
	for _, a := range r.Agents {
		if r.canDive(a) {
			names = append(names, "@"+a.Name())
		}
	}
	if len(names) == 0 {
		return "(none — grant tools: on a room agent)"
	}
	return strings.Join(names, ", ")
}

func (r *Room) agentByName(name string) *agent.Agent {
	for _, a := range r.Agents {
		if a.Name() == name {
			return a
		}
	}
	return nil
}
