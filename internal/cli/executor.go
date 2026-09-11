package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nerddevsltd/loop/internal/executor"
	"github.com/nerddevsltd/loop/internal/executor/cline"
)

// executorRegistry returns the registry the runner should use, with every
// built-in executor registered. Availability problems surface at run time
// with actionable errors (and proactively via `loop doctor`).
func executorRegistry() *executor.Registry {
	reg := executor.NewRegistry()
	reg.Register(cline.New())
	return reg
}

func newExecutorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "executor",
		Short: "Manage agent executors (engines that run agentic tasks)",
	}
	cmd.AddCommand(newExecutorInstallCmd())
	return cmd
}

func newExecutorInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <name>",
		Short: "Install an executor (currently: cline — requires Node 22+ and network)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "cline" {
				return fmt.Errorf("unknown executor %q (available: cline)", args[0])
			}
			dir, err := cline.Install()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ %v\n", err)
				os.Exit(1)
			}
			ex := cline.New()
			ex.HostPath = dir + "/index.mjs"
			fmt.Fprintf(cmd.OutOrStdout(), "✓ cline executor installed in %s\n", dir)
			if problems := ex.Check(); len(problems) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "warnings:")
				for _, p := range problems {
					fmt.Fprintf(cmd.ErrOrStderr(), "  - %v\n", p)
				}
			}
			return nil
		},
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check loop's prerequisites and report problems",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "checking loop prerequisites…")

			// Credentials source: a .env here is the usual setup.
			if _, err := os.Stat(".env"); err == nil {
				fmt.Fprintf(out, "✓ .env found in this directory (loaded at startup; shell env wins)\n")
			} else {
				fmt.Fprintf(out, "• no .env file (using shell environment only) — export ANTHROPIC_API_KEY / OPENAI_API_KEY\n")
			}

			cl := cline.New()
			problems := cl.Check()
			if len(problems) == 0 {
				fmt.Fprintf(out, "✓ cline executor: node + host + @cline/sdk all present\n")
				fmt.Fprintf(out, "  (node %s, host %s)\n", cl.NodeBin, cl.HostPath)
			} else {
				fmt.Fprintf(out, "✗ cline executor:\n")
				for _, p := range problems {
					fmt.Fprintf(out, "  - %v\n", p)
				}
				fmt.Fprintln(out, "  agent stages need this executor; pipelines using only")
				fmt.Fprintln(out, "  llm/tool/human/router stages work without it.")
			}
			return nil
		},
	}
}
