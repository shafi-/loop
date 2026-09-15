package config

import "testing"

// The resolver is loop's single precedence rule for model config:
// caller-provided values win, env fills only the gaps, built-in defaults
// are the floor. Every provider consumer routes through it.
func TestResolvePrecedence(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	t.Setenv("OPENAI_BASE_URL", "http://from-env:11434/v1")

	// Caller values win over env.
	rm := (&ModelConfig{
		Provider: ProviderOpenAI,
		Model:    "yaml-model",
		BaseURL:  "http://yaml:8080/v1",
	}).Resolve()
	if rm.Model != "yaml-model" || rm.BaseURL != "http://yaml:8080/v1" {
		t.Errorf("caller values must win over env: got %+v", rm)
	}

	// Env fills the gaps the caller left.
	rm = (&ModelConfig{Provider: ProviderAnthropic}).Resolve()
	if rm.Model != "claude-from-env" {
		t.Errorf("env must fill a missing model id: got %q", rm.Model)
	}
	if rm.BaseURL != "" {
		t.Errorf("no env for this endpoint means official (empty): got %q", rm.BaseURL)
	}
	if rm.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("api_key_env default must apply: got %q", rm.APIKeyEnv)
	}

	// Explicit api_key_env is never second-guessed.
	rm = (&ModelConfig{Provider: ProviderOpenAI, APIKeyEnv: "GROQ_API_KEY"}).Resolve()
	if rm.APIKeyEnv != "GROQ_API_KEY" {
		t.Errorf("explicit api_key_env must win: got %q", rm.APIKeyEnv)
	}

	// Provider is carried through untouched.
	if got := (&ModelConfig{Provider: ProviderOpenAI}).Resolve().Provider; got != "openai" {
		t.Errorf("provider must pass through: got %q", got)
	}

	// Known providers always resolve a usable model id.
	for _, p := range ValidProviders {
		if got := (&ModelConfig{Provider: p}).Resolve().Model; got == "" {
			t.Errorf("%s: resolution must never produce an empty model id", p)
		}
	}
}

// A nil model config is fully env-driven: provider family, model id, and
// key var all come from the environment (or its defaults). This is what
// YAML with no model block resolves to.
func TestResolveNilIsEnvDriven(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_MODEL", "env-model")

	var m *ModelConfig
	rm := m.Resolve()
	if rm.Provider != "openai" || rm.Model != "env-model" || rm.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("nil config must resolve from env: got %+v", rm)
	}
}
