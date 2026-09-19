// Package roomkit is the one recipe for hosting a room. loop chat and
// the daemon both host rooms; without this package each kept its own
// byte-for-byte copy of the agent, knowledge, and meter wiring, and the
// copies drifted only by luck. Every hosting surface assembles through
// here.
package roomkit

import (
	"fmt"
	"os"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/usage"
	"github.com/shafi-/loop/internal/workspace"
)

// Agents resolves a provider per agent of the room. An agent without a
// model block is env-driven: whatever family the environment configures
// (ModelConfig.Resolve fills in provider, model id, and key var).
// Agents with tools get the workspace as their working directory, and
// every agent carries the workspace brief so replies are about THIS
// project, not a generic one. When meter is non-nil, every provider is
// wrapped into it under the agent's name — /cost's per-agent numbers.
func Agents(room *config.Room, meter *usage.Meter) ([]*agent.Agent, error) {
	factory := engine.DefaultProviderFactory()
	cwd, _ := os.Getwd()
	brief := workspace.Brief(cwd)
	agents := make([]*agent.Agent, 0, len(room.Agents))
	for i := range room.Agents {
		p := room.Agents[i]
		model := p.Model
		if model == nil {
			model = &config.ModelConfig{}
		}
		provider, err := factory(model)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", p.Name, err)
		}
		provider = usage.Wrap(provider, meter, p.Name)
		agents = append(agents, &agent.Agent{Persona: p, Provider: provider, CWD: cwd, Workspace: brief})
	}
	return agents, nil
}

// KnowledgeManager builds the workspace's knowledge layer on the
// environment's provider — the grand rule, no flags.
func KnowledgeManager(meter *usage.Meter) (*knowledge.Manager, error) {
	cwd, _ := os.Getwd()
	mc := &config.ModelConfig{}
	p, err := engine.DefaultProviderFactory()(mc)
	if err != nil {
		return nil, err
	}
	return &knowledge.Manager{WS: cwd, Provider: p, Model: mc.Resolve().Model, Meter: meter}, nil
}

// AttachKnowledge wires the knowledge layer into a room: the /notes
// verb reads .loop/knowledge/, and every agent carries the digest
// block, re-read per reply so a refresh (a completed pipeline run, or
// a session-open reconcile) is seen by the next turn.
func AttachKnowledge(r *chat.Room, agents []*agent.Agent, meter *usage.Meter) error {
	cwd, _ := os.Getwd()
	r.Workspace = cwd
	km, err := KnowledgeManager(meter)
	if err != nil {
		return err
	}
	r.SetKnowledge(km)
	for _, a := range agents {
		a.Knowledge = func() string { return knowledge.DigestBlock(cwd) }
	}
	return nil
}
