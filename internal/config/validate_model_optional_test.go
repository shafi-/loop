package config

import "testing"

// The whole model layer is env-driven when YAML stays quiet: no model
// block at all, or an empty one, both validate — resolution fills them
// from the environment. Only contradictions (misspelled provider) error.
func TestValidateModelOptional(t *testing.T) {
	p, err := ParsePipeline([]byte(`
name: env-driven
stages:
  - id: a
    type: llm
    prompt: "say hi"
  - id: b
    type: llm
    model: {}
    prompt: "say hi again"
  - id: c
    type: agent
    persona: worker
    input: "ship it"
personas:
  - name: worker
    role: dev
    system: build it
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := p.Validate(); len(errs) > 0 {
		t.Fatalf("model-less stages should validate clean, got: %v", errs)
	}

	// A misspelled provider is still caught (ParsePipeline validates).
	if _, err := ParsePipeline([]byte(`
name: bad-provider
stages:
  - id: a
    type: llm
    model: {provider: gemini}
    prompt: "hi"
`)); err == nil {
		t.Fatal("misspelled provider must still be rejected")
	}
}
