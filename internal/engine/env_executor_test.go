package engine

import (
	"context"
	"testing"

	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// Agent stages hand a concrete ModelSpec to the executor. When no model
// is named anywhere in the YAML, the spec must carry the env-configured
// family, model, endpoint, and key — this is the executor-side half of
// env resolution (the LLM-stage half is covered by the httptest run).
func TestEnvDefaultsReachExecutorTask(t *testing.T) {
	ex := &recordingExecutor{out: "done"}
	reg := executor.NewRegistry()
	reg.Register(ex)

	t.Setenv("ANTHROPIC_MODEL", "claude-from-env")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example")
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	t.Setenv("OPENAI_MODEL", "")

	p := parse(t, `
name: env-agent
personas:
  - name: architect
    role: architect
    system: design it
stages:
  - id: design
    type: agent
    persona: architect
    executor: mock
    input: "do it"
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || len(ex.tasks) != 1 {
		t.Fatalf("res=%+v tasks=%d", res, len(ex.tasks))
	}
	got := ex.tasks[0].Model
	if got.Provider != "anthropic" {
		t.Errorf("provider = %q", got.Provider)
	}
	if got.Model != "claude-from-env" {
		t.Errorf("model = %q (want ANTHROPIC_MODEL)", got.Model)
	}
	if got.BaseURL != "https://gateway.example" {
		t.Errorf("base URL = %q (want ANTHROPIC_BASE_URL)", got.BaseURL)
	}
	if got.APIKey != "sk-from-env" {
		t.Errorf("api key must come from the env, got %q", got.APIKey)
	}

	// Explicit YAML still wins over the env on every field.
	p2 := parse(t, `
name: yaml-agent
personas:
  - name: architect
    role: architect
    system: design it
    model: {provider: anthropic, model: yaml-model}
stages:
  - id: design
    type: agent
    persona: architect
    executor: mock
    input: "do it"
`)
	ex2 := &recordingExecutor{out: "done"}
	reg2 := executor.NewRegistry()
	reg2.Register(ex2)
	r2 := &Runner{Pipeline: p2, Source: []byte("x"), Executors: reg2, RunsDir: t.TempDir()}
	if res, err := r2.Run(context.Background()); err != nil || !res.Completed {
		t.Fatalf("explicit run failed: res=%+v err=%v", res, err)
	}
	if got := ex2.tasks[0].Model.Model; got != "yaml-model" {
		t.Errorf("YAML model must beat ANTHROPIC_MODEL, got %q", got)
	}

	// Factory path sanity for the same stage shape: an llm stage next to
	// the agent one must resolve identically through the provider cache.
	m := llm.NewMock(&llm.Response{Text: "ok"})
	p3 := parse(t, `
name: mixed
stages:
  - id: spec
    type: tool
    run: echo spec
  - id: write
    type: llm
    prompt: "expand ${stages.spec.output}"
`)
	r3 := &Runner{Pipeline: p3, Source: []byte("x"), Providers: mockFactory(m), Executors: reg, RunsDir: t.TempDir()}
	if res, err := r3.Run(context.Background()); err != nil || !res.Completed {
		t.Fatalf("mixed run failed: res=%+v err=%v", res, err)
	}
	if got := m.Requests()[0].Model; got != "claude-from-env" {
		t.Errorf("model-less llm stage next to agent stage must use env model, got %q", got)
	}
}
