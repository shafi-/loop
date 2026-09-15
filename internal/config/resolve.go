package config

import "github.com/nerddevsltd/loop/internal/llm"

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

// Resolve applies loop's single precedence rule for model config:
//
//	caller-provided value > env override > built-in default
//
// Each caller hands in its own ModelConfig — a stage's YAML block, a
// persona's, or one assembled from command-line flags — and gets back
// the concrete values to use. Only the missing pieces are filled:
// explicit values are never second-guessed, env (ANTHROPIC_MODEL,
// OPENAI_MODEL, ANTHROPIC_BASE_URL, OPENAI_BASE_URL) fills gaps, and
// the built-in defaults are the floor.
func (m *ModelConfig) Resolve() ResolvedModel {
	if m == nil {
		return ResolvedModel{}
	}
	provider := string(m.Provider)
	if provider == "" {
		// Provider not named anywhere: follow whatever the environment
		// configures (llm.InferProvider). YAML validation rejects empty
		// providers, so today only code-constructed configs hit this —
		// but the rule belongs with the rest of resolution.
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
