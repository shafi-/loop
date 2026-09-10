package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validPipeline = `
name: feature-delivery
description: Turn an idea into a reviewed implementation plan
vars:
  idea: add OAuth login
personas:
  - name: architect
    role: Software architect
    system: You design systems. Be terse.
    model:
      provider: anthropic
      model: claude-sonnet-4-5
stages:
  - id: requirements
    type: llm
    model:
      provider: openai
      model: gpt-5
      base_url: http://localhost:11434/v1
      api_key_env: LOCAL_KEY
    prompt: "Turn this idea into requirements: {{ vars.idea }}"
    output: requirements_md
  - id: architecture
    type: agent
    persona: architect
    input: ${stages.requirements.output}
    tools: [read_file, write_file]
    max_iterations: 10
  - id: approval
    type: human
    prompt: "Approve architecture? ${stages.architecture.output}"
  - id: quality
    type: router
    when:
      - if: "${stages.approval.answer} == 'yes'"
        next: implement
      - next: requirements
  - id: implement
    type: tool
    run: ./scripts/build.sh
`

func TestLoadPipelineValid(t *testing.T) {
	p, err := LoadPipeline(writeTemp(t, "p.yaml", validPipeline))
	if err != nil {
		t.Fatalf("valid pipeline failed to load: %v", err)
	}
	if p.Name != "feature-delivery" {
		t.Errorf("Name = %q", p.Name)
	}
	if len(p.Stages) != 5 {
		t.Fatalf("got %d stages, want 5", len(p.Stages))
	}

	// Stage payload landed on the right variant.
	if p.Stages[0].LLM == nil || p.Stages[0].LLM.Prompt == "" {
		t.Error("stages[0] should be a populated llm stage")
	}
	if p.Stages[1].Agent == nil || p.Stages[1].Agent.Persona != "architect" {
		t.Error("stages[1] should be a populated agent stage")
	}
	if p.Stages[2].Human == nil {
		t.Error("stages[2] should be a human stage")
	}
	if p.Stages[3].Router == nil || len(p.Stages[3].Router.When) != 2 {
		t.Error("stages[3] should be a router stage with 2 rules")
	}
	if p.Stages[4].Tool == nil || p.Stages[4].Tool.Run != "./scripts/build.sh" {
		t.Error("stages[4] should be a tool stage")
	}

	// Output defaults.
	if p.Stages[0].LLM.Output != "requirements_md" {
		t.Errorf("explicit output lost: %q", p.Stages[0].LLM.Output)
	}
	if p.Stages[2].Human.Output != "approval.answer" {
		t.Errorf("human output default = %q, want %q", p.Stages[2].Human.Output, "approval.answer")
	}

	// api_key_env defaulting is provider-aware; explicit value preserved.
	if got := p.Stages[0].LLM.Model.APIKeyEnv; got != "LOCAL_KEY" {
		t.Errorf("explicit api_key_env = %q", got)
	}
	if p.Stages[0].LLM.Model.Provider != ProviderOpenAI || p.Stages[0].LLM.Model.BaseURL == "" {
		t.Error("openai provider should keep base_url")
	}

	// Persona model defaulted its key env from the provider.
	if got := p.Personas[0].Model.APIKeyEnv; got != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic api_key_env default = %q", got)
	}
}

func TestLoadPipelineRejectsUnknownField(t *testing.T) {
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: bad
stages:
  - id: a
    type: llm
    modle:
      provider: openai
      model: gpt-5
    prompt: hi
`))
	if err == nil {
		t.Fatal("typo'd field `modle` must be a load error")
	}
	if !strings.Contains(err.Error(), "modle") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
	// The message must be user-facing, not Go-reflection internals.
	if strings.Contains(err.Error(), "struct {") || strings.Contains(err.Error(), "config.") {
		t.Errorf("error leaks internals, got: %v", err)
	}
}

func TestLoadPipelineRejectsUnknownStageType(t *testing.T) {
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: bad
stages:
  - id: a
    type: magick
`))
	if err == nil || !strings.Contains(err.Error(), "magick") {
		t.Fatalf("unknown stage type must be rejected with its name, got: %v", err)
	}
}

func TestLoadPipelineTypeMismatchedFieldsRejected(t *testing.T) {
	// A router field on an llm stage must not pass silently.
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: bad
stages:
  - id: a
    type: llm
    model: { provider: openai, model: gpt-5 }
    prompt: hi
    when:
      - next: a
`))
	if err == nil || !strings.Contains(err.Error(), "when") {
		t.Fatalf("llm stage with router fields must be rejected, got: %v", err)
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: ""
stages:
  - id: requirements
    type: llm
    model: { provider: gemini, model: x }
    prompt: ""
  - id: requirements
    type: tool
  - id: gate
    type: router
    when:
      - next: nowhere
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"name: is required", "gemini", "prompt", "duplicates", "run", "nowhere"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("errors should mention %q, got:\n%v", want, err)
		}
	}
}

func TestValidateRouterWantsDefaultRule(t *testing.T) {
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: r
stages:
  - id: a
    type: llm
    model: { provider: openai, model: gpt-5 }
    prompt: hi
  - id: gate
    type: router
    when:
      - if: "x == 1"
        next: a
`))
	if err == nil || !strings.Contains(err.Error(), "default rule") {
		t.Fatalf("router without default rule should warn, got: %v", err)
	}
}

func TestValidateRetryAndOnError(t *testing.T) {
	_, err := LoadPipeline(writeTemp(t, "p.yaml", `
name: r
stages:
  - id: a
    type: tool
    run: echo hi
    on_error: explode
    retry:
      max_attempts: -1
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if !strings.Contains(err.Error(), "on_error") || !strings.Contains(err.Error(), "max_attempts") {
		t.Errorf("should report on_error and max_attempts, got: %v", err)
	}
}

const validRoom = `
name: leadership
agents:
  - name: ceo
    role: CEO
    system: You set direction and priorities.
    model: { provider: openai, model: gpt-5 }
  - name: cfo
    role: CFO
    system: You watch unit economics. Speak only when money is at stake.
    model: { provider: anthropic, model: claude-sonnet-4-5 }
  - name: architect
    role: Architect
    system: You care about systems and trade-offs.
settings:
  speak_threshold: 0.6
  max_spontaneous_replies: 2
  history_window: 50
`

func TestLoadRoomValid(t *testing.T) {
	r, err := LoadRoom(writeTemp(t, "room.yaml", validRoom))
	if err != nil {
		t.Fatalf("valid room failed to load: %v", err)
	}
	if r.Name != "leadership" || len(r.Agents) != 3 {
		t.Fatalf("room = %+v", r)
	}
	if got := r.Agents[0].Model.APIKeyEnv; got != "OPENAI_API_KEY" {
		t.Errorf("openai api_key_env default = %q", got)
	}
	// architect has no model — allowed, engine will apply a fallback later.
	if r.Agents[2].Model != nil {
		t.Error("architect should have no model")
	}
}

func TestLoadRoomErrors(t *testing.T) {
	_, err := LoadRoom(writeTemp(t, "room.yaml", `
name: ""
agents:
  - name: ceo
  - name: ceo
    model: { provider: grok, model: x }
settings:
  speak_threshold: 1.5
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"name: is required", "duplicates", "grok", "speak_threshold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("errors should mention %q, got:\n%v", want, err)
		}
	}
}

func TestLoadRoomRejectsUnknownField(t *testing.T) {
	_, err := LoadRoom(writeTemp(t, "room.yaml", `
name: r
agents:
  - name: a
sttings:
  speak_threshold: 0.5
`))
	if err == nil || !strings.Contains(err.Error(), "sttings") {
		t.Fatalf("typo'd settings key must be rejected, got: %v", err)
	}
}

func TestIdentifyKind(t *testing.T) {
	identify := func(doc string) (DocumentKind, bool) {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			t.Fatal(err)
		}
		return IdentifyKind(node.Content[0])
	}
	if kind, ok := identify(validPipeline); !ok || kind != KindPipeline {
		t.Errorf("pipeline doc identified as %q (ok=%v)", kind, ok)
	}
	if kind, ok := identify(validRoom); !ok || kind != KindRoom {
		t.Errorf("room doc identified as %q (ok=%v)", kind, ok)
	}
	if _, ok := identify("name: neither"); ok {
		t.Error("doc without stages/agents should not be identified")
	}
}
