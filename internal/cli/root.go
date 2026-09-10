// Package cli implements loop's command-line surface.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set via -ldflags at release time.
var version = "0.0.0-dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "loop",
		Short:         "Deterministic agentic harness: YAML pipelines + persona chat rooms",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newValidateCmd(),
		newInitCmd(),
		newAskCmd(),
		newNewCmd(),
	)
	return root
}

// Execute runs the root command and maps errors to exit codes.
func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
