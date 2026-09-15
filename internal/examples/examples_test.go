package examples

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// The example doubles as a few-shot prompt for the generator. If it ever
// stops validating, every generated pipeline would be taught a broken
// contract — so pin it.
func TestFeaturePipelineAlwaysValidates(t *testing.T) {
	if _, err := config.ParsePipeline([]byte(FeaturePipeline)); err != nil {
		t.Fatalf("embedded example pipeline no longer validates: %v", err)
	}
}

// The example deliberately demonstrates explicit model blocks (env-driven
// configs omit them), so guard that its agent stages keep them.
func TestFeaturePipelineAgentStagesHaveModels(t *testing.T) {
	p, err := config.ParsePipeline([]byte(FeaturePipeline))
	if err != nil {
		t.Fatal(err)
	}
	personas := map[string]*config.ModelConfig{}
	for i := range p.Personas {
		personas[p.Personas[i].Name] = p.Personas[i].Model
	}
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type != config.StageAgent {
			continue
		}
		if s.Agent.Model != nil {
			continue
		}
		if m := personas[s.Agent.Persona]; m != nil {
			continue
		}
		t.Errorf("agent stage %q (persona %q) lost its model block — the example teaches explicit models", s.ID, s.Agent.Persona)
	}
}

// stubExec stands in for the cline executor under its default name.
type stubExec struct {
	out   string
	calls int
}

func (e *stubExec) Name() string { return "cline" }
func (e *stubExec) Run(_ context.Context, _ executor.Task, _ func(executor.Event)) (*executor.Result, error) {
	e.calls++
	return &executor.Result{Output: e.out}, nil
}

// stubHumanUI answers human prompts from a script.
type stubHumanUI struct {
	answers []string
	asked   []string
}

func (h *stubHumanUI) Prompt(_ context.Context, prompt string) (string, error) {
	h.asked = append(h.asked, prompt)
	a := h.answers[0]
	h.answers = h.answers[1:]
	return a, nil
}

// The approval loop must listen: a rejection feeds the reviewer's words
// to a revise stage and re-asks — it must not redo requirements or the
// (expensive, agent-driven) architecture from scratch. Found in review
// of run 20260915-144628-f510, where the old loop re-ran the whole
// pipeline on every non-"yes" answer.
func TestFeaturePipelineRevisionLoopListens(t *testing.T) {
	p, err := config.ParsePipeline([]byte(FeaturePipeline))
	if err != nil {
		t.Fatal(err)
	}
	m := llm.NewMock(
		&llm.Response{Text: "REQUIREMENTS DOC", StopReason: llm.StopEndTurn},
		&llm.Response{Text: "changes", StopReason: llm.StopEndTurn}, // gate classifier
		&llm.Response{Text: "ARCHITECTURE v2 — revised", StopReason: llm.StopEndTurn},
	)
	ex := &stubExec{out: "ARCHITECTURE v1 — first draft"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	h := &stubHumanUI{answers: []string{"make it simpler", "yes"}}
	r := &engine.Runner{
		Pipeline:  p,
		Source:    []byte(FeaturePipeline),
		Providers: func(*config.ModelConfig) (llm.Provider, error) { return m, nil },
		Executors: reg,
		Human:     h,
		RunsDir:   t.TempDir(),
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("example run failed at %s: %v", res.FailedStage, res.Err)
	}
	// Three LLM calls total — requirements once, the gate's classifier
	// once, revise once. The second review's "yes" is vocabulary: no
	// call. A fourth means the loop re-ran requirements.
	if m.Calls() != 3 {
		t.Errorf("llm calls = %d, want 3 (requirements, classify, revise)", m.Calls())
	}
	// The agent ran exactly once: rejections must not redo its work.
	if ex.calls != 1 {
		t.Errorf("architecture runs = %d, want 1", ex.calls)
	}
	// The classifier saw the reviewer's words and the question asked.
	classifier := m.Requests()[1].Messages[0].Content
	if !strings.Contains(classifier, "make it simpler") || !strings.Contains(classifier, "ARCHITECTURE v1") {
		t.Errorf("classifier prompt missing the words or the plan under review:\n%s", classifier)
	}
	// The revise prompt carried the reviewer's words and the current plan.
	revise := m.Requests()[2].Messages[0].Content
	if !strings.Contains(revise, "make it simpler") || !strings.Contains(revise, "ARCHITECTURE v1") {
		t.Errorf("revise prompt missing reviewer words or current plan:\n%s", revise)
	}
	// The second review saw the revised plan, not the frozen first draft.
	if len(h.asked) != 2 || !strings.Contains(h.asked[1], "ARCHITECTURE v2") {
		t.Errorf("second review should show the revision:\n%s", strings.Join(h.asked, "\n---\n"))
	}
}
