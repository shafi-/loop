package llm

import "testing"

// The resolution contract: explicit YAML → env override → built-in
// default. Pinned here because every provider consumer (engine stages,
// agent replies, narrator, CLI commands) routes through these two
// functions and must agree.
func TestResolveModel(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	t.Setenv("OPENAI_MODEL", "gpt-from-env")

	if got := ResolveModel("anthropic", "explicit-model"); got != "explicit-model" {
		t.Errorf("explicit YAML value must win: got %q", got)
	}
	if got := ResolveModel("anthropic", ""); got != "claude-from-env" {
		t.Errorf("env override must apply when YAML is empty: got %q", got)
	}
	if got := ResolveModel("openai", ""); got != "gpt-from-env" {
		t.Errorf("openai env override must apply: got %q", got)
	}
	if got := ResolveModel("openai", "explicit-model"); got != "explicit-model" {
		t.Errorf("explicit beats env: got %q", got)
	}
}

func TestResolveModelDefaultFallback(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")

	if got := ResolveModel("anthropic", ""); got != DefaultModel["anthropic"] {
		t.Errorf("built-in default must apply without YAML or env: got %q", got)
	}
	if got := ResolveModel("openai", ""); got != DefaultModel["openai"] {
		t.Errorf("built-in default must apply without YAML or env: got %q", got)
	}
	// Known providers always resolve to something usable.
	for _, prov := range []string{"anthropic", "openai"} {
		if got := ResolveModel(prov, ""); got == "" {
			t.Errorf("%s resolution must never return an empty model id", prov)
		}
	}
	// Unknown providers resolve to "" here; validation rejects them
	// upstream with a proper error.
	if got := ResolveModel("gemini", ""); got != "" {
		t.Errorf("unknown providers have no default: got %q", got)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.corp/anthropic")
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")

	if got := ResolveBaseURL("anthropic", ""); got != "https://gateway.corp/anthropic" {
		t.Errorf("anthropic env endpoint must apply: got %q", got)
	}
	if got := ResolveBaseURL("openai", ""); got != "http://localhost:11434/v1" {
		t.Errorf("openai env endpoint must apply: got %q", got)
	}
	if got := ResolveBaseURL("openai", "https://yaml.example"); got != "https://yaml.example" {
		t.Errorf("explicit YAML endpoint must beat env: got %q", got)
	}
}

func TestResolveBaseURLEmptyMeansOfficial(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")

	if got := ResolveBaseURL("anthropic", ""); got != "" {
		t.Errorf("no env and no YAML must resolve to the official endpoint (empty): got %q", got)
	}
}

// InferProvider picks a provider family for callers that name none: an
// explicit PROVIDER env is the user's stated intention and wins; the
// single configured family wins next; both or neither configured falls
// back to the documented default (anthropic).
func TestInferProvider(t *testing.T) {
	reset := func() {
		t.Setenv("PROVIDER", "")
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("OPENAI_BASE_URL", "")
	}

	reset()
	if got := InferProvider(); got != "anthropic" {
		t.Errorf("nothing configured must fall back to anthropic: got %q", got)
	}

	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := InferProvider(); got != "openai" {
		t.Errorf("only openai configured must infer openai: got %q", got)
	}

	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	if got := InferProvider(); got != "openai" {
		t.Errorf("a bare OPENAI_BASE_URL must count as configured: got %q", got)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if got := InferProvider(); got != "anthropic" {
		t.Errorf("both configured must fall back to anthropic: got %q", got)
	}

	// PROVIDER is the stated intention: it wins over family inference
	// even when only the other family is configured.
	reset()
	t.Setenv("PROVIDER", "openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if got := InferProvider(); got != "openai" {
		t.Errorf("PROVIDER must override configured-family inference: got %q", got)
	}

	reset()
	t.Setenv("PROVIDER", "  Anthropic  ") // case and space tolerant
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if got := InferProvider(); got != "anthropic" {
		t.Errorf("PROVIDER must be trimmed and case-insensitive: got %q", got)
	}

	// Invalid values are surfaced, not silently swallowed.
	reset()
	t.Setenv("PROVIDER", "gemini")
	if got := InferProvider(); got != "gemini" {
		t.Errorf("invalid PROVIDER must pass through to error surfaces: got %q", got)
	}
}
