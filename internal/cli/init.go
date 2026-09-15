package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

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
				filepath.Join(dir, "pipelines", "feature-delivery.yaml"): examples.FeaturePipeline,
				filepath.Join(dir, "rooms", "leadership.yaml"):           assetRoom,
				filepath.Join(dir, "README.md"):                          assetReadme,
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
			fmt.Fprintf(cmd.OutOrStdout(), "\nWorkspace ready (%d file(s)). Next:\n  loop validate pipelines/feature-delivery.yaml\n  loop validate rooms/leadership.yaml\n", written)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}
