package engine

import (
	"fmt"
	"os"
	"sync"

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/llm"
)

// ProviderFactory turns a stage's model config into a ready provider.
// The indirection exists so tests can inject mock providers.
type ProviderFactory func(cfg *config.ModelConfig) (llm.Provider, error)

// DefaultProviderFactory builds real providers from the environment and
// caches them: a pipeline with many stages on the same provider reuses one
// client (and its connection pool).
func DefaultProviderFactory() ProviderFactory {
	var mu sync.Mutex
	cache := map[string]llm.Provider{}
	return func(cfg *config.ModelConfig) (llm.Provider, error) {
		rm := cfg.Resolve()
		key := fmt.Sprintf("%s|%s|%s", rm.Provider, rm.BaseURL, rm.APIKeyEnv)
		mu.Lock()
		defer mu.Unlock()
		if p, ok := cache[key]; ok {
			return p, nil
		}
		apiKey := os.Getenv(rm.APIKeyEnv)
		if apiKey == "" && rm.BaseURL == "" {
			return nil, fmt.Errorf("model %s: env var %s is not set (provider %q requires it — set the key, or switch this model block to your configured provider)", rm.Model, rm.APIKeyEnv, rm.Provider)
		}
		p, err := llm.New(rm.Provider, llm.Options{
			APIKey:      apiKey,
			BaseURL:     rm.BaseURL,
			MaxAttempts: 3,
			BackoffMs:   500,
		})
		if err != nil {
			return nil, err
		}
		cache[key] = p
		return p, nil
	}
}

// resolveAPIKey reads the configured env var for a model config. Agent
// stages hand keys to executors directly; llm stages go through the
// factory's providers. A custom endpoint (YAML base_url or the provider's
// BASE_URL env) makes the key optional — local/keyless servers exist.
func resolveAPIKey(cfg *config.ModelConfig) (string, error) {
	rm := cfg.Resolve()
	key := os.Getenv(rm.APIKeyEnv)
	if key == "" && rm.BaseURL == "" {
		return "", fmt.Errorf("env var %s is not set (provider %q requires it — set the key, or switch this model block to your configured provider)", rm.APIKeyEnv, rm.Provider)
	}
	return key, nil
}

// llmRequest converts a stage model config into a normalized request
// header (model params only; messages come from the stage). Resolution
// happens inside the config resolver.
func llmRequest(cfg *config.ModelConfig, system string, messages []llm.Message) llm.Request {
	if cfg == nil {
		cfg = &config.ModelConfig{}
	}
	rm := cfg.Resolve()
	return llm.Request{
		Model:       rm.Model,
		System:      system,
		Messages:    messages,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
	}
}
