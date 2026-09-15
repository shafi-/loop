package config

import (
	"testing"

	"github.com/nerddevsltd/loop/internal/llm"
)

// The grand rule, pinned: .env/env configures the baseline, and any
// pipeline, room, or command may override any piece of it — or run fully
// self-contained. This test walks one pipeline containing all three
// postures side by side and asserts each field lands where the rule says.
func TestGrandRulePartialOverrides(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "https://gw-env.example/v1")
	t.Setenv("OPENAI_MODEL", "env-model")

	p, err := ParsePipeline([]byte(`
name: grand-rule
stages:
  # Self-contained: everything explicit, env ignored entirely.
  - id: pinned
    type: llm
    model: {provider: anthropic, model: pin-a, base_url: "https://own.example", api_key_env: OWN_KEY}
    prompt: "hi"
  # Partial override: only the model id is named; family, endpoint, and
  # key var all come from the environment.
  - id: partial
    type: llm
    model: {model: pin-b}
    prompt: "hi"
  # Partial override, the other direction: family named, model id inherited.
  - id: family-only
    type: llm
    model: {provider: anthropic}
    prompt: "hi"
  # No model block at all: fully env-driven.
  - id: bare
    type: llm
    prompt: "hi"
  # Key var overridden alone: family still inferred from env.
  - id: key-only
    type: llm
    model: {api_key_env: MY_KEY}
    prompt: "hi"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	model := func(id string) ResolvedModel {
		for i := range p.Stages {
			if p.Stages[i].ID == id {
				return p.Stages[i].LLM.Model.Resolve()
			}
		}
		t.Fatalf("stage %q not found", id)
		return ResolvedModel{}
	}

	if m := model("pinned"); m.Provider != "anthropic" || m.Model != "pin-a" || m.BaseURL != "https://own.example" || m.APIKeyEnv != "OWN_KEY" {
		t.Errorf("pinned stage must be fully self-contained: %+v", m)
	}
	if m := model("partial"); m.Provider != "openai" || m.Model != "pin-b" || m.BaseURL != "https://gw-env.example/v1" || m.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("partial stage: model pinned, rest must come from env: %+v", m)
	}
	if m := model("family-only"); m.Provider != "anthropic" {
		// Provider explicit; the model id defers: ANTHROPIC_MODEL is unset
		// here, so it must land on the built-in default (checked next).
		t.Errorf("family-only stage: provider must be explicit anthropic: %+v", m)
	}
	if m := model("family-only"); m.Model != llm.DefaultModel["anthropic"] {
		t.Errorf("family-only stage model must fall to the built-in default: %+v", m)
	}
	if m := model("bare"); m.Provider != "openai" || m.Model != "env-model" || m.BaseURL != "https://gw-env.example/v1" {
		t.Errorf("bare stage must be fully env-driven: %+v", m)
	}
	if m := model("key-only"); m.Provider != "openai" || m.APIKeyEnv != "MY_KEY" {
		t.Errorf("key-only stage: family inferred from env, key var explicit: %+v", m)
	}
}
