package config

import (
	"os"
	"strings"
)

// This file is loop's model-resolution vocabulary: which env vars name
// the intended family, model, and endpoint, and how a ModelConfig turns
// into a fully-resolved model when some pieces are missing. It lives in
// config — not llm — so the domain tier owns the whole rule and the
// transport tier only ever receives resolved values.

// Per-provider env overrides, read by ResolveModel/ResolveBaseURL. Set
// them in the shell or .env and pipelines/rooms need no model block
// details in YAML — just `provider:` and these.
const (
	EnvAnthropicModel   = "ANTHROPIC_MODEL"
	EnvOpenAIModel      = "OPENAI_MODEL"
	EnvAnthropicBaseURL = "ANTHROPIC_BASE_URL"
	EnvOpenAIBaseURL    = "OPENAI_BASE_URL"

	// EnvProvider names the intended provider family ("anthropic" or
	// "openai", case-insensitive). It is the env-level intention: above
	// family inference, below explicit YAML/flags.
	EnvProvider = "PROVIDER"
)

// DefaultModel is the floor of the resolution rule: the model id used
// when neither YAML nor the environment names one.
var DefaultModel = map[Provider]string{
	"anthropic": "claude-sonnet-4-5",
	"openai":    "gpt-5",
}

func envModelName(provider Provider) string {
	if provider == "anthropic" {
		return EnvAnthropicModel
	}
	return EnvOpenAIModel
}

func envBaseURLName(provider Provider) string {
	if provider == "anthropic" {
		return EnvAnthropicBaseURL
	}
	return EnvOpenAIBaseURL
}

// ResolveModel picks the model id for a provider: explicit YAML value,
// then the provider's env override, then the built-in default. Never
// returns "" for a known provider — the built-in default always exists.
// Unknown providers resolve to "" and are rejected at validation.
func ResolveModel(provider Provider, explicit string) string {
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
func ResolveBaseURL(provider Provider, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return os.Getenv(envBaseURLName(provider))
}

// InferProvider picks a provider family for callers that name none:
// an explicit PROVIDER env value is the user's stated intention and wins;
// otherwise, if exactly one family is configured (key or BASE_URL set),
// that family is used; with both or neither configured, anthropic stays
// the default. Explicit YAML/flags still outrank all of this.
// An invalid PROVIDER value is returned as-is so the consumer's error
// names it instead of silently falling back.
func InferProvider() Provider {
	if p := strings.ToLower(strings.TrimSpace(os.Getenv(EnvProvider))); p != "" {
		return Provider(p)
	}
	anthropicConfigured := os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv(EnvAnthropicBaseURL) != ""
	openaiConfigured := os.Getenv("OPENAI_API_KEY") != "" || os.Getenv(EnvOpenAIBaseURL) != ""
	if openaiConfigured && !anthropicConfigured {
		return "openai"
	}
	return "anthropic"
}
