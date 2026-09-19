package cli

import (
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// resolveProvider builds a provider from command-line flags. Flag values
// become the caller-provided ModelConfig; llm.Build applies the shared
// precedence (flags > env > defaults), the shared key rule, and the
// shared retry policy. Returns the provider, the resolved provider name,
// and the resolved model id.
func resolveProvider(provider, model, baseURL, apiKeyEnv string, retries int) (llm.Provider, string, string, error) {
	cfg := &config.ModelConfig{
		Provider:  config.Provider(provider),
		Model:     model,
		BaseURL:   baseURL,
		APIKeyEnv: apiKeyEnv,
	}
	p, err := llm.Build(cfg, retries, 500)
	if err != nil {
		return nil, "", "", err
	}
	rm := cfg.Resolve()
	return p, rm.Provider, rm.Model, nil
}
