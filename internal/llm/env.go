package llm

import "os"

// Per-provider env overrides, read by ResolveModel/ResolveBaseURL. Set
// them in the shell or .env and pipelines/rooms need no model block
// details in YAML — just `provider:` and these.
const (
	EnvAnthropicModel   = "ANTHROPIC_MODEL"
	EnvOpenAIModel      = "OPENAI_MODEL"
	EnvAnthropicBaseURL = "ANTHROPIC_BASE_URL"
	EnvOpenAIBaseURL    = "OPENAI_BASE_URL"
)

func envModelName(provider string) string {
	if provider == "anthropic" {
		return EnvAnthropicModel
	}
	return EnvOpenAIModel
}

func envBaseURLName(provider string) string {
	if provider == "anthropic" {
		return EnvAnthropicBaseURL
	}
	return EnvOpenAIBaseURL
}

// ResolveModel picks the model id for a provider: explicit YAML value,
// then the provider's env override, then the built-in default. Never
// returns "" for a known provider — the built-in default always exists.
// Unknown providers resolve to "" and are rejected at validation.
func ResolveModel(provider, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv(envModelName(provider)); v != "" {
		return v
	}
	return DefaultModel[provider]
}

// ResolveBaseURL picks the API endpoint: explicit YAML value, then the
// provider's env override, then "" meaning the official endpoint.
func ResolveBaseURL(provider, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv(envBaseURLName(provider))
}

// InferProvider picks a provider family for callers that name none:
// if exactly one family is configured (key or BASE_URL set), that family
// wins; with both or neither configured, anthropic stays the default.
// Explicit choices — the --provider flag, a YAML provider field — always
// take precedence over this.
func InferProvider() string {
	anthropicConfigured := os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv(EnvAnthropicBaseURL) != ""
	openaiConfigured := os.Getenv("OPENAI_API_KEY") != "" || os.Getenv(EnvOpenAIBaseURL) != ""
	if openaiConfigured && !anthropicConfigured {
		return "openai"
	}
	return "anthropic"
}
