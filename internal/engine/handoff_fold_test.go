package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// The engine-side hand-off fold: a run started by a room carries
// room_context=<brief path>; the first model-facing stage receives the
// brief even when its pipeline never reads the var — the var alone was
// an opt-in contract, and pipelines that ignore it ran on baked-in
// defaults (the room asked for one thing, the run built the demo).

func foldTestPipeline(t *testing.T, vars, input string) *config.Pipeline {
	t.Helper()
	return parse(t, fmt.Sprintf(`
name: fold-demo
%s
personas:
  - name: architect
    role: Architect
    system: You design systems.
stages:
  - id: build
    type: agent
    executor: mock
    persona: architect
    input: %s
`, vars, input))
}

func writeBrief(t *testing.T, dir string) string {
	t.Helper()
	brief := filepath.Join(dir, "handoff.md")
	if err := os.WriteFile(brief, []byte("task: add rate limiting\n\nthe room decided: token bucket, no new dependencies"), 0o644); err != nil {
		t.Fatal(err)
	}
	return brief
}

const foldMarker = "--- room context (from the room that started this run) ---"

func eventsOf(t *testing.T, runsDir, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runsDir, runID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHandoffFoldsIntoFirstAgentStage(t *testing.T) {
	dir := t.TempDir()
	brief := writeBrief(t, dir)
	ex := &recordingExecutor{out: "done"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := foldTestPipeline(t, fmt.Sprintf("vars: {room_context: %s}", brief), `"implement the task"`)

	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	if len(ex.tasks) != 1 {
		t.Fatalf("executor calls = %d", len(ex.tasks))
	}
	got := ex.tasks[0].Instruction
	if !strings.HasPrefix(got, "implement the task\n\n"+foldMarker) {
		t.Errorf("brief not folded onto the input:\n%s", got)
	}
	for _, want := range []string{"token bucket, no new dependencies", "authoritative statement of the task"} {
		if !strings.Contains(got, want) {
			t.Errorf("folded input missing %q:\n%s", want, got)
		}
	}
	// The fold is audited, with the brief's path.
	if ev := eventsOf(t, dir, res.RunID); !strings.Contains(ev, "handoff_folded") || !strings.Contains(ev, brief) {
		t.Errorf("fold not audited:\n%s", ev)
	}
}

// A pipeline that reads the brief itself (the starter implement
// pipeline's room-context stage) must not get it twice: the fold skips
// stages whose template already references the var.
func TestHandoffSelfDeliveredPipelineSkipsFold(t *testing.T) {
	dir := t.TempDir()
	brief := writeBrief(t, dir)
	ex := &recordingExecutor{out: "done"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := foldTestPipeline(t, fmt.Sprintf("vars: {room_context: %s}", brief), `"implement; brief at ${vars.room_context}"`)

	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	if got := ex.tasks[0].Instruction; strings.Contains(got, foldMarker) {
		t.Errorf("self-delivering pipeline got the brief twice:\n%s", got)
	}
	if ev := eventsOf(t, dir, res.RunID); !strings.Contains(ev, "handoff_selfdelivered") {
		t.Errorf("self-delivery not audited:\n%s", ev)
	}
}

// Exactly one delivery per run: the second model stage never sees the
// brief again (it is already in the run's upstream outputs if needed).
func TestHandoffFoldsOnceAcrossStages(t *testing.T) {
	dir := t.TempDir()
	brief := writeBrief(t, dir)
	m := llm.NewMock(&llm.Response{Text: "ok"})
	p := parse(t, fmt.Sprintf(`
name: fold-once
vars: {room_context: %s}
stages:
  - id: one
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "first"
  - id: two
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "second ${stages.one.output}"
`, brief))
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: mockFactory(m), RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d", len(reqs))
	}
	if n := strings.Count(reqs[0].Messages[0].Content, foldMarker); n != 1 {
		t.Errorf("first stage fold count = %d\n%s", n, reqs[0].Messages[0].Content)
	}
	if strings.Contains(reqs[1].Messages[0].Content, foldMarker) {
		t.Errorf("second stage got the brief again:\n%s", reqs[1].Messages[0].Content)
	}
}

// An unreadable brief degrades deterministically: the run proceeds
// without the fold, and the skip is audited.
func TestHandoffUnreadableBriefSkips(t *testing.T) {
	dir := t.TempDir()
	ex := &recordingExecutor{out: "done"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	missing := filepath.Join(dir, "gone", "handoff.md")
	p := foldTestPipeline(t, fmt.Sprintf("vars: {room_context: %s}", missing), `"implement the task"`)

	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	if got := ex.tasks[0].Instruction; got != "implement the task" {
		t.Errorf("input changed despite an unreadable brief:\n%s", got)
	}
	if ev := eventsOf(t, dir, res.RunID); !strings.Contains(ev, "handoff_skipped") {
		t.Errorf("skip not audited:\n%s", ev)
	}
}

// No room_context var, no fold — runs started outside a room are
// byte-identical to before.
func TestHandoffAbsentOutsideRooms(t *testing.T) {
	ex := &recordingExecutor{out: "done"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := foldTestPipeline(t, "vars: {}", `"implement the task"`)

	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	if got := ex.tasks[0].Instruction; got != "implement the task" {
		t.Errorf("input changed without a room brief:\n%s", got)
	}
}

// A retried stage re-folds its freshly rendered input — the brief must
// not vanish on attempt two, nor appear twice in one prompt.
type foldRetryProvider struct {
	failFirst bool
	requests  []llm.Request
}

func (p *foldRetryProvider) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.requests = append(p.requests, req)
	if p.failFirst {
		p.failFirst = false
		return nil, errors.New("transient")
	}
	return &llm.Response{Text: "ok"}, nil
}

func (p *foldRetryProvider) Stream(ctx context.Context, req llm.Request, onDelta llm.StreamFunc) (*llm.Response, error) {
	return p.Complete(ctx, req)
}

func TestHandoffFoldSurvivesStageRetry(t *testing.T) {
	dir := t.TempDir()
	brief := writeBrief(t, dir)
	prov := &foldRetryProvider{failFirst: true}
	p := parse(t, fmt.Sprintf(`
name: fold-retry
vars: {room_context: %s}
stages:
  - id: work
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "do the work"
    retry: {max_attempts: 3, backoff_ms: 1}
`, brief))
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: func(*config.ModelConfig) (llm.Provider, error) { return prov, nil }, RunsDir: dir, CWD: dir}
	res, err := r.Run(context.Background())
	if err != nil || !res.Completed {
		t.Fatalf("run: %v %+v", err, res)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider calls = %d, want the failed attempt and the retry", len(prov.requests))
	}
	for i, req := range prov.requests {
		if n := strings.Count(req.Messages[0].Content, foldMarker); n != 1 {
			t.Errorf("attempt %d fold count = %d\n%s", i+1, n, req.Messages[0].Content)
		}
	}
	// The audit line stays singular across retries.
	if n := strings.Count(eventsOf(t, dir, res.RunID), "handoff_folded"); n != 1 {
		t.Errorf("fold audited %d times across a retry, want once", n)
	}
}
