package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/usage"
)

// newKnowledgeManager builds the workspace's knowledge maintainer: the
// env-default model (the same fallback any persona without a model
// block uses), metered under the "knowledge" label so /cost shows what
// maintenance spent.
func newKnowledgeManager(meter *usage.Meter) (*knowledge.Manager, error) {
	cwd, _ := os.Getwd()
	mc := &config.ModelConfig{}
	p, err := engine.DefaultProviderFactory()(mc)
	if err != nil {
		return nil, err
	}
	return &knowledge.Manager{WS: cwd, Provider: p, Model: mc.Resolve().Model, Meter: meter}, nil
}

// attachKnowledge wires the knowledge layer into a room: the /notes
// verb and post-turn refreshes read and write .loop/knowledge/, and
// every agent carries the digest block, re-read per reply so a
// mid-session refresh is seen by the next turn. An unresolvable
// provider returns an error — the knowledge layer is off, not fatal.
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

// maintainWorkspaceKnowledge refreshes the knowledge layer after a
// pipeline run — the auto-maintained briefs catching up with an
// implementation the run just landed. Incremental: an unchanged
// workspace costs zero model calls and stays silent.
func maintainWorkspaceKnowledge(ctx context.Context, logf func(string, ...any)) {
	km, err := newKnowledgeManager(usage.NewMeter())
	if err != nil {
		return // no provider configured — silently off
	}
	if line := km.MaintainLine(ctx); line != "" {
		logf("%s", line)
	}
}

func newDigestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "digest",
		Short: "Rebuild the workspace's maintained project knowledge (digest + area notes)",
		Long: `Rebuild the workspace's maintained project knowledge.

The knowledge layer lives in .loop/knowledge/: a digest that rides every
room agent's system prompt, and per-area notes agents pull on demand.
Maintenance is incremental — an unchanged workspace costs zero model
calls. Rooms maintain it automatically after implementation turns; this
command does it headless (from scripts, or after pipeline runs).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			km, err := newKnowledgeManager(usage.NewMeter())
			if err != nil {
				return fmt.Errorf("the knowledge layer needs a provider: %w", err)
			}
			res, err := km.Maintain(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "◈ %s\n", res.Message)
			for _, u := range res.Updated {
				fmt.Fprintf(out, "  · note %s\n", u)
			}
			if res.DigestUpdated {
				fmt.Fprintln(out, "  · digest")
			}
			for _, d := range res.Deferred {
				fmt.Fprintf(out, "  · deferred %s (call cap — run again to continue)\n", d)
			}
			for _, f := range res.Failed {
				fmt.Fprintf(out, "  ✗ %s\n", f)
			}
			fmt.Fprintln(out, "  files: .loop/knowledge/ (digest.md, notes/, index.json)")
			return nil
		},
	}
}
