package config

import "testing"

// The model id inside a model block is optional: resolution falls back to
// the ANTHROPIC_MODEL / OPENAI_MODEL env, then built-in defaults. A
// base_url is valid for both provider styles (env endpoints made the old
// openai-only restriction incoherent).
func TestValidateModelIDOptional(t *testing.T) {
	p, err := ParsePipeline([]byte(`
name: no-model-ids
stages:
  - id: a
    type: llm
    model:
      provider: anthropic
      base_url: https://gateway.corp/anthropic
    prompt: "say hi"
  - id: b
    type: agent
    persona: worker
    input: "ship it"
personas:
  - name: worker
    role: dev
    system: build it
    model:
      provider: openai
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := p.Validate(); len(errs) > 0 {
		t.Fatalf("model ids and anthropic base_url should validate clean, got: %v", errs)
	}
}
