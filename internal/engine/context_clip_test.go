package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// Values interpolated into prompts clip at MaxPromptValue with a
// visible marker; the context keeps the full value; plain Interpolate
// (tools, routers) stays unclipped.
func TestInterpolatePromptClipsLargeValues(t *testing.T) {
	c := NewContext(nil)
	big := strings.Repeat("a", MaxPromptValue+5000)
	c.SetOutput("gen", "output", big)

	// Plain Interpolate: verbatim.
	got, err := c.Interpolate("${stages.gen.output}")
	if err != nil || got != big {
		t.Fatalf("plain interpolate must be verbatim: len=%d err=%v", len(got), err)
	}
	// Prompt interpolation: clipped with a marker saying so.
	got, err = c.InterpolatePrompt("${stages.gen.output}")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxPromptValue+256 {
		t.Errorf("prompt value len = %d, want ≤ max+marker", len(got))
	}
	if !strings.Contains(got, "value clipped") {
		t.Error("clipped value must carry the marker")
	}
	// The clip is reported for the run log.
	if clipped := c.TakeClipped(); len(clipped) != 1 || clipped[0] != "stages.gen.output" {
		t.Errorf("TakeClipped = %v", clipped)
	}
	// Small values pass untouched and report nothing.
	c.SetOutput("smol", "output", "tiny")
	if got, _ := c.InterpolatePrompt("${stages.smol.output} x"); got != "tiny x" {
		t.Errorf("small value disturbed: %q", got)
	}
	if clipped := c.TakeClipped(); clipped != nil {
		t.Errorf("small value must not report clips: %v", clipped)
	}
}

// An llm stage referencing a huge stage output sends the clipped prompt
// to the provider and a value_clipped event to the run log.
func TestLLMStageClipsAndLogs(t *testing.T) {
	dir := t.TempDir()
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		if len(req.Messages[0].Content) > MaxPromptValue+512 {
			t.Errorf("provider got an unclipped prompt: %d bytes", len(req.Messages[0].Content))
		}
		return &llm.Response{Text: "ok"}
	})
	src := `
name: clip-demo
stages:
  - id: ask
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "summarize: ${stages.gen.output}"
`
	p := parse(t, src)
	log, err := CreateRunLog(dir, "clip-run", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	d := &stageDeps{Providers: mockFactory(m), Log: log, Warnf: func(string, ...any) {}}
	c := NewContext(nil)
	c.SetOutput("gen", "output", strings.Repeat("b", MaxPromptValue+2048))

	if _, err := runLLMStage(context.Background(), &p.Stages[0], c, d); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "clip-run", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "value_clipped") {
		t.Error("run log missing the value_clipped event")
	}
}

// max_iterations reaches the executor — it was parsed and silently
// ignored before.
func TestAgentStageMaxIterationsWired(t *testing.T) {
	var got executor.Task
	reg := fakeRegistry(t, func(_ context.Context, task executor.Task, _ func(executor.Event)) (*executor.Result, error) {
		got = task
		return &executor.Result{Output: "done"}, nil
	})
	p := parse(t, `
name: turns
personas:
  - name: worker
    system: "You work."
stages:
  - id: w
    type: agent
    persona: worker
    input: "do it"
    max_iterations: 7
`)
	d := &stageDeps{CWD: t.TempDir(), Executors: reg, Pipeline: p}
	outcome, err := runAgentStage(context.Background(), &p.Stages[0], NewContext(nil), d)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Output != "done" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got.MaxTurns != 7 {
		t.Fatalf("MaxTurns = %d, want 7 (max_iterations must be wired)", got.MaxTurns)
	}
}

// The gate classifier sees at most the question's tail.
func TestGateClassifierClipsQuestion(t *testing.T) {
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		if len(req.Messages[0].Content) > (8<<10)+1024 {
			t.Errorf("classifier payload = %d bytes, want ≤ 8K+slack", len(req.Messages[0].Content))
		}
		return &llm.Response{Text: "yes"}
	})
	s := &config.Stage{ID: "gate", Human: &config.HumanStage{}}
	d := &stageDeps{Providers: mockFactory(m)}
	intent, via, err := classifyIntent(context.Background(), s, d, strings.Repeat("q", 100_000), "words words")
	if err != nil || intent != "yes" || via != "llm" {
		t.Fatalf("intent=%q via=%q err=%v", intent, via, err)
	}
}

func fakeRegistry(t *testing.T, run func(_ context.Context, task executor.Task, _ func(executor.Event)) (*executor.Result, error)) *executor.Registry {
	t.Helper()
	reg := executor.NewRegistry()
	reg.Register(fakeExecutor{name: "cline", run: run})
	return reg
}

type fakeExecutor struct {
	name string
	run  func(ctx context.Context, task executor.Task, onEvent func(executor.Event)) (*executor.Result, error)
}

func (f fakeExecutor) Name() string { return f.name }

func (f fakeExecutor) Run(ctx context.Context, task executor.Task, onEvent func(executor.Event)) (*executor.Result, error) {
	return f.run(ctx, task, onEvent)
}
