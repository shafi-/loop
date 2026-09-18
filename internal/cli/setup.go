package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/executor/cline"
	"github.com/shafi-/loop/internal/llm"
)

// newSetupCmd is the onboarding command: make this installation ready
// to use. Idempotent — safe to run again after upgrades.
func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Onboard this installation: providers, agent executor, health check",
		Long: `Prepares loop for use:

  1. Providers — checks your environment (shell + .env) for a usable
     model family and prints what every model-less config would resolve
     to; missing keys get copy-paste instructions.
  2. Agent executor — installs the cline host, preferring a standalone
     compiled binary (fetches the bun toolchain once, ~30 MB, when
     neither bun nor Node 22+ is present). After this, agent stages run
     with no runtime dependencies.
  3. Doctor — the final health check.

Run it again any time; it only fills what is missing or refreshes what
changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "loop setup")

			// 1. Providers.
			fmt.Fprintln(out, "\n── providers ──")
			switch family := configuredFamily(); family {
			case "":
				fmt.Fprintln(out, "• no API key or endpoint configured yet")
				fmt.Fprintln(out, "  copy .env.sample to .env and set ONE key:")
				fmt.Fprintln(out, "    cp .env.sample .env    # then fill ANTHROPIC_API_KEY or OPENAI_API_KEY")
				fmt.Fprintln(out, "  (pipelines of llm/tool/human/router stages need a key; local")
				fmt.Fprintln(out, "   servers via *_BASE_URL work keyless — see USER MANUAL.md)")
			default:
				rm := (&config.ModelConfig{Provider: config.Provider(family)}).Resolve()
				fmt.Fprintf(out, "✓ %s configured — model-less configs resolve to %s", family, rm.Model)
				if rm.BaseURL != "" {
					fmt.Fprintf(out, " at %s", rm.BaseURL)
				}
				fmt.Fprintln(out)
			}

			// 2. Agent executor.
			fmt.Fprintln(out, "\n── agent executor (cline) ──")
			dir, installErr := cline.Install()
			ex := cline.New()
			problems := ex.Check()
			switch {
			case installErr == nil && len(problems) == 0:
				fmt.Fprintf(out, "✓ host files in %s\n", dir)
				if ex.StandaloneHost() {
					fmt.Fprintln(out, "✓ standalone host compiled — agent stages need no Node or bun at runtime")
				} else {
					fmt.Fprintf(out, "✓ script mode ready (node: %s)\n", ex.NodeBin)
				}
			case len(problems) == 0:
				// The refresh failed, but what's installed still works.
				fmt.Fprintf(cmd.ErrOrStderr(), "! executor refresh failed (%v) — the existing install still works\n", installErr)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "✗ executor install: %v\n", installErr)
				for _, p := range problems {
					fmt.Fprintf(cmd.ErrOrStderr(), "✗ %v\n", p)
				}
				return fmt.Errorf("executor is not ready — fix the problems above and re-run `loop setup`")
			}

			// 3. Shipped personas (the dev team every project can reference).
			fmt.Fprintln(out, "\n── built-in personas ──")
			seedGlobalPersonas(out, cmd.ErrOrStderr())

			// 4. Doctor.
			fmt.Fprintln(out, "\n── health check ──")
			if err := runDoctor(out, cmd.ErrOrStderr()); err != nil {
				return err
			}
			fmt.Fprintln(out, "\nready. next:")
			fmt.Fprintln(out, "  loop init                      # scaffold a workspace (if you haven't)")
			fmt.Fprintln(out, "  loop chat rooms/feature.yaml   # shape a feature with the shipped council")
			fmt.Fprintln(out, "  loop chat rooms/dev.yaml       # the shipped dev team")
			fmt.Fprintln(out, "  loop run pipelines/review.yaml")
			return nil
		},
	}
}

// configuredFamily reports which provider family the environment
// actually configures ("" when none): a key or an explicit BASE_URL per
// family counts; PROVIDER alone does not (it states intent, not keys).
func configuredFamily() string {
	aKey := os.Getenv("ANTHROPIC_API_KEY") != ""
	oKey := os.Getenv("OPENAI_API_KEY") != ""
	aURL := os.Getenv("ANTHROPIC_BASE_URL") != ""
	oURL := os.Getenv("OPENAI_BASE_URL") != ""
	switch {
	case aKey && !oKey:
		return "anthropic"
	case oKey && !aKey:
		return "openai"
	case aKey && oKey:
		if p := llm.InferProvider(); p == "openai" {
			return "openai"
		}
		return "anthropic"
	case aURL:
		return "anthropic"
	case oURL:
		return "openai"
	}
	return ""
}
