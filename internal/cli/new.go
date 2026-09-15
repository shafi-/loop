// Package cli implements loop's command-line surface.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nerddevsltd/loop/internal/generator"
)

func newNewCmd() *cobra.Command {
	var (
		out        string
		provider   string
		model      string
		baseURL    string
		apiKeyEnv  string
		maxRepairs int
		quiet      bool
	)
	cmd := &cobra.Command{
		Use:   "new <description>",
		Short: "Generate a pipeline from a plain-language description (LLM drafts, loop validates)",
		Long: `Describe the process you want in plain language; an LLM drafts the
pipeline and loop's own validator checks it. Validation errors are fed
back to the model for repair. You always get validated YAML — or an
honest failure. Hand-written pipelines are equally first-class.`,
		Example: `  loop new "triage GitHub issues daily: label bugs, draft fixes, PR for review" -o pipelines/triage.yaml`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, provName, model, err := resolveProvider(provider, model, baseURL, apiKeyEnv, 3)
			if err != nil {
				return err
			}
			logf := func(format string, a ...any) {
				if !quiet {
					fmt.Fprintf(cmd.ErrOrStderr(), "· "+format+"\n", a...)
				}
			}
			g := &generator.Generator{Provider: p, ProviderName: provName, Model: model, MaxRepairs: maxRepairs, Logf: logf}
			logf("drafting pipeline with %s (%s)...", provName, model)
			res, err := g.Generate(context.Background(), args[0])
			if err != nil {
				return err
			}

			name := res.Pipeline.Name
			if out == "" {
				_, err = cmd.OutOrStdout().Write(res.YAML)
				return err
			}
			path := out
			if fi, err := os.Stat(path); err == nil && fi.IsDir() {
				path = filepath.Join(path, name+".yaml")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, res.YAML, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "✓ wrote %s (%d stages)\n", path, len(res.Pipeline.Stages))
			fmt.Fprintf(cmd.OutOrStdout(), "next: loop validate %s && loop run %s\n", path, path)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "write YAML here (file or directory); default: stdout")
	f.StringVar(&provider, "provider", "", "provider: anthropic or openai (default: follows your configured env, else anthropic)")
	f.StringVar(&model, "model", "", "model id (default: provider-specific)")
	f.StringVar(&baseURL, "base-url", "", "override API base URL (OpenAI shape)")
	f.StringVar(&apiKeyEnv, "api-key-env", "", "env var holding the API key")
	f.IntVar(&maxRepairs, "max-repairs", 2, "validation-repair passes before giving up")
	f.BoolVarP(&quiet, "quiet", "q", false, "suppress progress lines on stderr")
	cmd.Long = strings.TrimSpace(cmd.Long)
	return cmd
}
