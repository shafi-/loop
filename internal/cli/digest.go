package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/roomkit"
	"github.com/shafi-/loop/internal/usage"
)

// maintainWorkspaceKnowledge refreshes the knowledge layer after a
// pipeline run — the auto-maintained briefs catching up with an
// implementation the run just landed. Incremental: an unchanged
// workspace costs zero model calls and stays silent.
func maintainWorkspaceKnowledge(ctx context.Context, logf func(string, ...any)) {
	km, err := roomkit.KnowledgeManager(usage.NewMeter())
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
calls. Completed pipeline runs refresh it automatically; rooms reconcile
it at session open. This command does it headless (from scripts, or any
time you want the summaries now).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			km, err := roomkit.KnowledgeManager(usage.NewMeter())
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
