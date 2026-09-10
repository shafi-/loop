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
		key := fmt.Sprintf("%s|%s|%s", cfg.Provider, cfg.BaseURL, cfg.APIKeyEnv)
		mu.Lock()
		defer mu.Unlock()
		if p, ok := cache[key]; ok {
			return p, nil
		}
		apiKey := os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" && cfg.BaseURL == "" {
			return nil, fmt.Errorf("model %s: env var %s is not set", cfg.Model, cfg.APIKeyEnv)
		}
		p, err := llm.New(string(cfg.Provider), llm.Options{
			APIKey:      apiKey,
			BaseURL:     cfg.BaseURL,
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
// factory's providers.
func resolveAPIKey(cfg *config.ModelConfig) (string, error) {
	key := os.Getenv(cfg.APIKeyEnv)
	if key == "" && cfg.BaseURL == "" {
		return "", fmt.Errorf("env var %s is not set", cfg.APIKeyEnv)
	}
	return key, nil
}

// llmRequest converts a stage model config into a normalized request
// header (model params only; messages come from the stage).
func llmRequest(cfg *config.ModelConfig, system string, messages []llm.Message) llm.Request {
	return llm.Request{
		Model:       cfg.Model,
		System:      system,
		Messages:    messages,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
	}
}
