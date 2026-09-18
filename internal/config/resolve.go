package config

import "github.com/shafi-/loop/internal/llm"

// ResolvedModel is a ModelConfig with every optional piece filled in.
// Nothing here is "" for a valid provider, so callers never re-derive
// defaults. Secrets are deliberately absent: API keys are read from the
// environment at call time, never resolved into structs that could be
// logged or snapshotted.
type ResolvedModel struct {
	Provider  string
	Model     string
	BaseURL   string // "" = official endpoint
	APIKeyEnv string
}

// Resolve applies loop's grand rule for model configuration — the same
// rule at every consumer (commands, pipeline stages, room agents, the
// narrator):
//
//  1. Explicit values win. YAML model blocks and CLI flags may override
//     partially: naming a model inherits provider/endpoint/key from
//     below; naming a provider inherits the model id. A config may also
//     be fully self-contained with no environment at all.
//  2. The environment fills what's left. The shell beats the .env file
//     (dotenv only fills gaps). PROVIDER names the intended family
//     outright; without it, the configured family supplies the provider,
//     the model (ANTHROPIC_MODEL / OPENAI_MODEL), and the endpoint
//     (ANTHROPIC_BASE_URL / OPENAI_BASE_URL).
//  3. Built-in defaults are the floor: official endpoints,
//     claude-sonnet-4-5 / gpt-5, family-specific key vars.
//
// Only the missing pieces are filled; explicit values are never
// second-guessed.
func (m *ModelConfig) Resolve() ResolvedModel {
	if m == nil {
		// No model config at all = fully env-driven: the configured
		// provider family, its env model, its default key var.
		m = &ModelConfig{}
	}
	provider := string(m.Provider)
	if provider == "" {
		// Provider not named anywhere: follow whatever the environment
		// configures (llm.InferProvider). This is the env-first rule the
		// whole file exists for — YAML names a family only to override it.
		provider = llm.InferProvider()
	}
	apiKeyEnv := m.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = DefaultAPIKeyEnv[Provider(provider)]
	}
	return ResolvedModel{
		Provider:  provider,
		Model:     llm.ResolveModel(provider, m.Model),
		BaseURL:   llm.ResolveBaseURL(provider, m.BaseURL),
		APIKeyEnv: apiKeyEnv,
	}
}
