package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nerddevsltd/loop/internal/llm"
)

func newAskCmd() *cobra.Command {
	var (
		provider   string
		model      string
		baseURL    string
		apiKeyEnv  string
		system     string
		noStream   bool
		maxRetries int
	)
	cmd := &cobra.Command{
		Use:   "ask <prompt>",
		Short: "Send one prompt to an LLM provider and stream the answer (provider smoke test)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, model, err := resolveProvider(provider, model, baseURL, apiKeyEnv, maxRetries)
			if err != nil {
				return err
			}

			req := llm.Request{
				Model:    model,
				System:   system,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: args[0]}},
			}
			ctx := context.Background()
			if noStream {
				resp, err := p.Complete(ctx, req)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), resp.Text)
				return nil
			}
			out := cmd.OutOrStdout()
			resp, err := p.Stream(ctx, req, func(delta string) {
				fmt.Fprint(out, delta)
			})
			fmt.Fprintln(out)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\n[%s | in:%d out:%d tokens]\n", resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "provider: anthropic or openai (default: follows your configured env, else anthropic)")
	cmd.Flags().StringVar(&model, "model", "", fmt.Sprintf("model id (default: provider-specific, e.g. %s)", "claude-sonnet-4-5 / gpt-5"))
	cmd.Flags().StringVar(&baseURL, "base-url", "", "override API base URL (OpenAI shape: Ollama, Groq, vLLM, ...)")
	cmd.Flags().StringVar(&apiKeyEnv, "api-key-env", "", "env var holding the API key (default: provider-specific)")
	cmd.Flags().StringVar(&system, "system", "", "optional system prompt")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "wait for the full response instead of streaming")
	cmd.Flags().IntVar(&maxRetries, "retries", 3, "total attempts on transient failures")
	return cmd
}
