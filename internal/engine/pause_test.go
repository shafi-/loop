package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// pausingHuman mimics a user typing /pause at the first prompt.
type pausingHuman struct{ asked int }

func (h *pausingHuman) Prompt(_ context.Context, _ string) (string, error) {
	h.asked++
	return "", ErrPaused
}

func readEvents(t *testing.T, dir, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, runID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readStateFile(t *testing.T, dir, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, runID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const pausePipeline = `name: gate
stages:
  - id: draft
    type: tool
    run: echo "draft v"
  - id: approval
    type: human
    prompt: "Approve ${stages.draft.output}?"
  - id: ship
    type: tool
    run: echo shipped
`

func TestHumanPauseIsCleanAndResumable(t *testing.T) {
	p := parse(t, pausePipeline)
	dir := t.TempDir()

	h := &pausingHuman{}
	r := &Runner{Pipeline: p, Source: []byte(pausePipeline), Human: h, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || res.FailedStage != "" || res.Err != nil {
		t.Fatalf("pause misreported as failure: %+v", res)
	}
	if !res.Paused || res.PausedStage != "approval" {
		t.Fatalf("res = %+v, want paused at approval", res)
	}
	// A pause is not a failure in the state file, and it records its
	// resume point.
	if st := readStateFile(t, dir, res.RunID); !strings.Contains(st, `"paused": "approval"`) || strings.Contains(st, `"failed"`) {
		t.Errorf("state after pause = %s", st)
	}
	if ev := readEvents(t, dir, res.RunID); !strings.Contains(ev, "run_paused") || strings.Contains(ev, "stage_failed") {
		t.Errorf("events after pause = %s", ev)
	}

	// Resume with an agreeing human: draft is skipped, approval re-runs.
	h2 := &stubHuman{answers: []string{"yes"}}
	r2 := &Runner{ResumeID: res.RunID, Human: h2, RunsDir: dir}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Completed {
		t.Fatalf("resume after pause failed at %s: %v", res2.FailedStage, res2.Err)
	}
	if len(h2.asked) != 1 || !strings.Contains(h2.asked[0], "draft v") {
		t.Errorf("resume prompts = %v", h2.asked)
	}
}

func TestPauseIsNotRetried(t *testing.T) {
	p := parse(t, `name: retry-pause
stages:
  - id: approval
    type: human
    prompt: ok?
    retry: {max_attempts: 3, backoff_ms: 1}
`)
	h := &pausingHuman{}
	r := &Runner{Pipeline: p, Source: []byte("x"), Human: h, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paused {
		t.Fatalf("res = %+v, want paused", res)
	}
	if h.asked != 1 {
		t.Errorf("prompt count = %d, want 1 (a pause must not be retried)", h.asked)
	}
}

func TestInterruptMarksResumePoint(t *testing.T) {
	const src = `name: interrupted
stages:
  - id: one
    type: llm
    model: {provider: anthropic, model: test}
    prompt: hi
  - id: two
    type: tool
    run: echo done
`
	p := parse(t, src)
	ctx, cancel := context.WithCancel(context.Background())
	m := llm.NewMockFunc(func(llm.Request) *llm.Response {
		cancel() // the interrupt lands after stage one succeeds
		return &llm.Response{Text: "ok", StopReason: llm.StopEndTurn}
	})
	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte(src), Providers: func(*config.ModelConfig) (llm.Provider, error) { return m, nil }, RunsDir: dir}
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || !res.Paused || res.PausedStage != "two" || res.Err == nil {
		t.Fatalf("res = %+v, want interrupted-paused at two with cause", res)
	}
	if st := readStateFile(t, dir, res.RunID); !strings.Contains(st, `"paused": "two"`) {
		t.Errorf("interrupt must record a resume point: %s", st)
	}

	// Resume with a live context: stage one is skipped, two runs.
	r2 := &Runner{ResumeID: res.RunID, RunsDir: dir}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Completed {
		t.Fatalf("resume after interrupt failed at %s: %v", res2.FailedStage, res2.Err)
	}
}

func TestHumanPromptAndAnswerAreLogged(t *testing.T) {
	h := &stubHuman{answers: []string{"make it shorter"}}
	p := parse(t, `name: audit
stages:
  - id: spec
    type: tool
    run: echo spec-doc
  - id: approval
    type: human
    prompt: "Approve ${stages.spec.output}?"
  - id: ship
    type: tool
    run: echo shipped
`)
	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte("x"), Human: h, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("run failed: %v", res.Err)
	}
	ev := readEvents(t, dir, res.RunID)
	for _, want := range []string{`"type":"human_prompt"`, "Approve spec-doc?", `"type":"human_answer"`, "make it shorter"} {
		if !strings.Contains(ev, want) {
			t.Errorf("run log missing %q — audit trail must record both sides of the exchange", want)
		}
	}
}

// streamingExecutor feeds text deltas through onEvent like the cline
// host does, to prove the deltas land in the run log with content.
type streamingExecutor struct{}

func (streamingExecutor) Name() string { return "cline" }
func (streamingExecutor) Run(_ context.Context, _ executor.Task, onEvent func(executor.Event)) (*executor.Result, error) {
	onEvent(executor.Event{Type: executor.EventText, Text: "the architecture doc, streamed"})
	return &executor.Result{Output: "the architecture doc, streamed"}, nil
}

func TestExecutorTextEventsCarryPayloadInRunLog(t *testing.T) {
	reg := executor.NewRegistry()
	reg.Register(streamingExecutor{})
	p := parse(t, `name: stream-audit
personas:
  - name: architect
    role: Architect
    system: You design systems.
    model: {provider: anthropic, model: persona-model}
stages:
  - id: design
    type: agent
    persona: architect
    input: design it
`)
	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, Stdout: io.Discard, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("run failed: %v", res.Err)
	}
	ev := readEvents(t, dir, res.RunID)
	if !strings.Contains(ev, `"detail":"the architecture doc, streamed"`) {
		t.Errorf("text events must log their payload (audit trail), got:\n%s", ev)
	}
}
