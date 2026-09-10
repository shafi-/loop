package cli

import (
	"fmt"
	"os"

	"github.com/nerddevsltd/loop/internal/llm"
)

// resolveProvider builds a provider from command-line flags, applying the
// same defaults everywhere: provider-specific key env vars and model ids,
// with a clear error when nothing can authenticate. Returns the provider
// and the resolved model id.
func resolveProvider(provider, model, baseURL, apiKeyEnv string, retries int) (llm.Provider, string, error) {
	if apiKeyEnv == "" {
		apiKeyEnv = llm.DefaultAPIKeyEnvName[provider]
	}
	opts := llm.Options{BaseURL: baseURL, MaxAttempts: retries}
	if apiKeyEnv != "" {
		opts.APIKey = os.Getenv(apiKeyEnv)
	}
	if opts.APIKey == "" && baseURL == "" {
		return nil, "", fmt.Errorf("no API key: set %s (or pass --base-url for a local/keyless server)", apiKeyEnv)
	}
	if model == "" {
		model = llm.DefaultModel[provider]
	}
	p, err := llm.New(provider, opts)
	if err != nil {
		return nil, "", err
	}
	return p, model, nil
}
