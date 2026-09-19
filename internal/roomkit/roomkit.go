// Package roomkit is the one recipe for hosting a room. loop chat and
// the daemon both host rooms; without this package each kept its own
// byte-for-byte copy of the agent, knowledge, and meter wiring, and the
// copies drifted only by luck. Every hosting surface assembles through
// here.
package roomkit

import (
	"context"
	"fmt"
	"os"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/llm"
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
func AttachKnowledge(r *chat.Room, meter *usage.Meter) error {
	cwd, _ := os.Getwd()
	r.Workspace = cwd
	km, err := KnowledgeManager(meter)
	if err != nil {
		return err
	}
	r.SetKnowledge(km)
	for _, a := range r.Agents {
		a.Knowledge = func() string { return knowledge.DigestBlock(cwd) }
	}
	return nil
}

// Room assembles a hosted room end to end: agents on metered providers,
// the session ledger, and the host ports — /cost reads pipeline spend
// through the engine's run records, /compact summarizes through the
// lead agent. The one assembly every hosting surface shares.
func Room(cfg *config.Room, tr *chat.Transcript, meter *usage.Meter) (*chat.Room, error) {
	agents, err := Agents(cfg, meter)
	if err != nil {
		return nil, err
	}
	r := chat.NewRoom(*cfg, agents, tr)
	r.Usage = meter
	r.RunUsage = engine.ReadRunUsage
	if len(agents) > 0 {
		r.Summarizer = summarizer(agents[0], meter)
	}
	return r, nil
}

// compactVoice is /compact's summarizer brief: one tight paragraph,
// no ceremony.
const compactVoice = `You summarize a strategy-room session for its participants.
One tight paragraph: what was decided, what is open, facts established,
and what remains. No preamble, no bullet-point ceremony.`

// summarizer binds /compact's one model call to an agent: its provider
// (already metered per agent), re-labeled so the summary shows up as
// its own line in /cost.
func summarizer(a *agent.Agent, meter *usage.Meter) func(context.Context, string) (string, error) {
	return func(ctx context.Context, text string) (string, error) {
		provider := a.Provider
		if meter != nil {
			provider = usage.Wrap(provider, meter, "compact")
		}
		resp, err := provider.Complete(ctx, llm.Request{
			Model:     a.Model(),
			System:    compactVoice,
			Messages:  []llm.Message{{Role: llm.RoleUser, Content: text + "\n\nSummarize this session so far."}},
			MaxTokens: 700,
		})
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	}
}
