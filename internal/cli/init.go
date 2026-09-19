package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/counters"
	"github.com/shafi-/loop/internal/examples"
)

func newInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a loop workspace with example pipeline and room",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			files := map[string]string{
				filepath.Join(dir, config.PipelinesDir, "feature-delivery.yaml"): examples.FeaturePipeline,
				filepath.Join(dir, config.PipelinesDir, "implement.yaml"):        examples.ImplementPipeline,
				filepath.Join(dir, config.PipelinesDir, "review.yaml"):           examples.ReviewPipeline,
				filepath.Join(dir, config.RoomsDir, "leadership.yaml"):           assetRoom,
				filepath.Join(dir, config.RoomsDir, "feature.yaml"):              examples.FeatureRoom,
				filepath.Join(dir, config.RoomsDir, "dev.yaml"):                  examples.DevRoom,
				filepath.Join(dir, "README.md"):                                  assetReadme,
			}
			written := 0
			for path, content := range files {
				if _, err := os.Stat(path); err == nil && !force {
					fmt.Fprintf(cmd.ErrOrStderr(), "• %s already exists, skipping (use --force to overwrite)\n", path)
					continue
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				// 0o644: config files are world-readable, not executable.
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ wrote %s\n", path)
				written++
			}
			// The shipped dev team: global personas every project can
			// reference (.loop/rooms/dev.yaml and the pipelines do).
			seedGlobalPersonas(cmd.OutOrStdout(), cmd.ErrOrStderr())
			hintLegacyDirs(cmd.ErrOrStderr(), dir)
			fmt.Fprintf(cmd.OutOrStdout(), "\nWorkspace ready (%d file(s)). Next:\n"+
				"  loop chat .loop/rooms/feature.yaml \"shape: one-off invoices for freelancers\"\n"+
				"  loop chat .loop/rooms/dev.yaml \"plan: add a health endpoint\"\n"+
				"  loop run .loop/pipelines/review.yaml\n", written)
			// Opt-in, anonymous, local (internal/counters).
			counters.Bump("workspaces_initialized")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}

// hintLegacyDirs warns about pre-.loop workspaces: loop now keeps every
// artifact under .loop/, and files in the old root-level directories
// are simply not seen anymore. A workspace scaffolded before the move
// needs its directories moved once.
func hintLegacyDirs(errOut io.Writer, dir string) {
	var legacy []string
	for _, d := range []string{"rooms", "pipelines", "personas", "plans"} {
		entries, err := os.ReadDir(filepath.Join(dir, d))
		if err != nil || len(entries) == 0 {
			continue
		}
		legacy = append(legacy, d)
	}
	if len(legacy) == 0 {
		return
	}
	fmt.Fprintf(errOut, "! %s holds loop artifacts in the old root-level layout: %s\n"+
		"  move them once and everything picks them up:\n"+
		"  mkdir -p %s && mv %s %s/\n",
		dir, strings.Join(legacy, ", "),
		filepath.Join(dir, config.LoopDir), strings.Join(legacy, " "), filepath.Join(dir, config.LoopDir))
}
