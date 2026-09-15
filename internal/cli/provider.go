package cli

import (
	"fmt"
	"os"

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/llm"
)

// resolveProvider builds a provider from command-line flags. Flag values
// become the caller-provided ModelConfig; the config resolver applies the
// shared precedence (flags > env > defaults), inferring the provider
// family when none is named, and a clear error is returned when nothing
// can authenticate. Returns the provider, the resolved provider name,
// and the resolved model id.
func resolveProvider(provider, model, baseURL, apiKeyEnv string, retries int) (llm.Provider, string, string, error) {
	rm := (&config.ModelConfig{
		Provider:  config.Provider(provider),
		Model:     model,
		BaseURL:   baseURL,
		APIKeyEnv: apiKeyEnv,
	}).Resolve()
	opts := llm.Options{BaseURL: rm.BaseURL, MaxAttempts: retries}
	opts.APIKey = os.Getenv(rm.APIKeyEnv)
	if opts.APIKey == "" && rm.BaseURL == "" {
		return nil, "", "", fmt.Errorf("no API key: set %s (or pass --base-url for a local/keyless server)", rm.APIKeyEnv)
	}
	p, err := llm.New(rm.Provider, opts)
	if err != nil {
		return nil, "", "", err
	}
	return p, rm.Provider, rm.Model, nil
}
