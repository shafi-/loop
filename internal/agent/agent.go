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

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/llm"
)

// Agent is one room participant: a persona with a resolved provider.
type Agent struct {
	Persona  config.Persona
	Provider llm.Provider
}

// Name returns the persona identifier.
func (a *Agent) Name() string { return a.Persona.Name }

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

// Reply produces this agent's answer to the conversation. The transcript
// is explicitly attributed ([name] lines) because provider role arrays
// cannot represent multi-party conversations cleanly. When onDelta is
// non-nil the reply streams through it as it is generated.
func (a *Agent) Reply(ctx context.Context, room []config.Persona, conversation string, onDelta llm.StreamFunc) (string, error) {
	system := strings.TrimSpace(a.Persona.System + "\n\n" + a.framing(room) + `
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

// DecideSpeak asks whether this agent should respond to the new message.
// It uses structured output: cheap (few hundred tokens), typed, and
// identical across providers.
func (a *Agent) DecideSpeak(ctx context.Context, room []config.Persona, conversation, newMessage string) (Decision, error) {
	role := a.Persona.Role
	if role == "" {
		role = "colleague"
	}
	system := fmt.Sprintf("You are %s, the %s. You decide whether to speak.", a.Persona.Name, role)
	prompt := fmt.Sprintf(`%s

=== New message from the user ===
%s

Should you (%s) respond to this new message? Judge from your role's
perspective only: does the user genuinely need input from a %s right
now? Responding without real value is noise. Silence is respectable.`,
		conversation, newMessage, a.Persona.Name, role)

	req := llm.Request{
		Model:          a.model(),
		System:         system,
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		ResponseSchema: speakSchema,
		MaxTokens:      200,
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
	if a.Persona.Model == nil {
		return ""
	}
	// Env override (ANTHROPIC_MODEL / OPENAI_MODEL) and the built-in
	// default apply when the persona names only a provider.
	return llm.ResolveModel(string(a.Persona.Model.Provider), a.Persona.Model.Model)
}
