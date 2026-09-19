package engine

import (
	"os"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// ProviderFactory turns a stage's model config into a ready provider.
// The indirection exists so tests can inject mock providers.
type ProviderFactory = llm.Factory

// DefaultProviderFactory builds real providers through llm's shared
// construction policy — the environment fills the config, a missing key
// is an explained error, and identical configs reuse one client (and
// its connection pool).
func DefaultProviderFactory() ProviderFactory { return llm.NewFactory() }

// resolveAPIKey reads the configured env var for a model config. Agent
// stages hand keys to executors directly; llm stages go through the
// factory's providers. The key rule itself is llm.CheckKey's.
func resolveAPIKey(cfg *config.ModelConfig) (string, error) {
	rm := cfg.Resolve()
	if err := llm.CheckKey(rm); err != nil {
		return "", err
	}
	return os.Getenv(rm.APIKeyEnv), nil
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
