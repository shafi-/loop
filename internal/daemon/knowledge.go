package daemon

import (
	"os"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/usage"
)

// newKnowledgeManager builds the workspace's knowledge maintainer — the
// same rule as the chat command: the env-default model, metered under
// the "knowledge" label.
func newKnowledgeManager(meter *usage.Meter) (*knowledge.Manager, error) {
	cwd, _ := os.Getwd()
	mc := &config.ModelConfig{}
	p, err := engine.DefaultProviderFactory()(mc)
	if err != nil {
		return nil, err
	}
	return &knowledge.Manager{WS: cwd, Provider: p, Model: mc.Resolve().Model, Meter: meter}, nil
}

// attachKnowledge wires the knowledge layer into a hosted room: the
// digest rides every agent's system prompt (re-read per reply, so a
// refresh is seen by the next turn) and /notes reads it for zero
// tokens. Refreshes come from completed pipeline runs and session-open
// reconciliation — agent file writes mid-conversation never touch it.
func attachKnowledge(r *chat.Room, agents []*agent.Agent, meter *usage.Meter) error {
	cwd, _ := os.Getwd()
	r.Workspace = cwd
	km, err := newKnowledgeManager(meter)
	if err != nil {
		return err
	}
	r.SetKnowledge(km)
	for _, a := range agents {
		a.Knowledge = func() string { return knowledge.DigestBlock(cwd) }
	}
	return nil
}
