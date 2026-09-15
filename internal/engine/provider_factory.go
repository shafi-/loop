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
		// Env overrides (ANTHROPIC_BASE_URL / OPENAI_BASE_URL) count as
		// "custom endpoint" everywhere cfg.BaseURL does, so they must be
		// resolved before the cache key and the keyless check.
		baseURL := llm.ResolveBaseURL(string(cfg.Provider), cfg.BaseURL)
		key := fmt.Sprintf("%s|%s|%s", cfg.Provider, baseURL, cfg.APIKeyEnv)
		mu.Lock()
		defer mu.Unlock()
		if p, ok := cache[key]; ok {
			return p, nil
		}
		apiKey := os.Getenv(cfg.APIKeyEnv)
		if apiKey == "" && baseURL == "" {
			return nil, fmt.Errorf("model %s: env var %s is not set", llm.ResolveModel(string(cfg.Provider), cfg.Model), cfg.APIKeyEnv)
		}
		p, err := llm.New(string(cfg.Provider), llm.Options{
			APIKey:      apiKey,
			BaseURL:     baseURL,
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
	key := os.Getenv(cfg.APIKeyEnv)
	if key == "" && llm.ResolveBaseURL(string(cfg.Provider), cfg.BaseURL) == "" {
		return "", fmt.Errorf("env var %s is not set", cfg.APIKeyEnv)
	}
	return key, nil
}

// llmRequest converts a stage model config into a normalized request
// header (model params only; messages come from the stage). Model ids
// resolve here so YAML, env (ANTHROPIC_MODEL / OPENAI_MODEL), and the
// built-in default fall through in that order.
func llmRequest(cfg *config.ModelConfig, system string, messages []llm.Message) llm.Request {
	return llm.Request{
		Model:       llm.ResolveModel(string(cfg.Provider), cfg.Model),
		System:      system,
		Messages:    messages,
		Temperature: cfg.Temperature,
		MaxTokens:   cfg.MaxTokens,
	}
}
