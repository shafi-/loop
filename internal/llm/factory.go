package llm

import (
	"fmt"
	"os"
	"sync"

	"github.com/shafi-/loop/internal/config"
)

// This file is loop's one provider-construction policy. Every call site
// — pipeline stages, room agents, CLI commands, the daemon's authoring —
// gets its clients from here, so the key rule, the retry policy, and the
// client cache cannot drift apart again.

// Factory turns a model config into a ready provider. The indirection
// exists so tests can inject mock providers.
type Factory func(cfg *config.ModelConfig) (Provider, error)

// NewFactory returns a Factory with its own client cache: a pipeline or
// room with many stages on the same provider reuses one client (and its
// connection pool).
func NewFactory() Factory {
	var mu sync.Mutex
	cache := map[string]Provider{}
	return func(cfg *config.ModelConfig) (Provider, error) {
		rm := cfg.Resolve()
		key := rm.Provider + "|" + rm.BaseURL + "|" + rm.APIKeyEnv
		mu.Lock()
		defer mu.Unlock()
		if p, ok := cache[key]; ok {
			return p, nil
		}
		p, err := Build(cfg, 3, 500)
		if err != nil {
			return nil, err
		}
		cache[key] = p
		return p, nil
	}
}

// Build constructs one provider from a model config: the config
// resolver fills the gaps, CheckKey explains a missing key, and retry
// rides the client. Callers that construct repeatedly should prefer
// NewFactory. A zero or negative maxAttempts means no retry.
func Build(cfg *config.ModelConfig, maxAttempts, backoffMs int) (Provider, error) {
	rm := cfg.Resolve()
	if err := CheckKey(rm); err != nil {
		return nil, err
	}
	return New(rm.Provider, Options{
		APIKey:      os.Getenv(rm.APIKeyEnv),
		BaseURL:     rm.BaseURL,
		MaxAttempts: maxAttempts,
		BackoffMs:   backoffMs,
	})
}

// CheckKey reports the standard missing-key error for a resolved model.
// A custom endpoint (YAML base_url or the provider's BASE_URL env)
// makes the key optional — local/keyless servers exist — and the mock
// provider is keyless by design.
func CheckKey(rm config.ResolvedModel) error {
	if rm.Provider == "mock" || os.Getenv(rm.APIKeyEnv) != "" || rm.BaseURL != "" {
		return nil
	}
	return fmt.Errorf("model %s: env var %s is not set (provider %q requires it — set the key, or switch this model block to your configured provider)", rm.Model, rm.APIKeyEnv, rm.Provider)
}
