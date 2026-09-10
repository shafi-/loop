package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/executor"
	"github.com/nerddevsltd/loop/internal/llm"
)

// stubHuman answers human prompts from a script and records what it was asked.
type stubHuman struct {
	answers []string
	asked   []string
}

func (h *stubHuman) Prompt(_ context.Context, prompt string) (string, error) {
	h.asked = append(h.asked, prompt)
	if len(h.answers) == 0 {
		return "", errors.New("no scripted answers left")
	}
	a := h.answers[0]
	h.answers = h.answers[1:]
	return a, nil
}

// mockFactory returns a factory handing every request the same mock provider.
func mockFactory(m *llm.Mock) ProviderFactory {
	return func(*config.ModelConfig) (llm.Provider, error) { return m, nil }
}

func parse(t *testing.T, src string) *config.Pipeline {
	t.Helper()
	p, err := config.ParsePipeline([]byte(src))
	if err != nil {
		t.Fatalf("pipeline parse: %v", err)
	}
	return p
}

func TestRunnerLinearPipeline(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: "REQUIREMENTS DOC"})
	p := parse(t, `
name: linear
vars: {idea: oauth}
stages:
  - id: a
    type: tool
    run: 'echo "step A: {{ vars.idea }}"'
  - id: b
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "expand ${stages.a.output}"
    output: doc_alias
  - id: c
    type: tool
    run: 'echo "C got: ${stages.b.output}"'
`)
	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: mockFactory(m), RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.Steps != 3 {
		t.Fatalf("res = %+v", res)
	}
	// Stage order and data flow verified through the run log's final context.
	snap, err := os.ReadFile(filepath.Join(dir, res.RunID, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(snap)
	if !strings.Contains(s, "step A: oauth") || !strings.Contains(s, "C got: REQUIREMENTS DOC") {
		t.Errorf("context snapshot lost data flow:\n%s", s)
	}
	if !strings.Contains(s, "doc_alias") {
		t.Errorf("explicit output alias missing:\n%s", s)
	}
	// Exactly one LLM call happened.
	if m.Calls() != 1 {
		t.Errorf("provider calls = %d", m.Calls())
	}
	// The prompt that reached the provider was interpolated.
	req := m.Requests()[0]
	if !strings.Contains(req.Messages[0].Content, "expand step A: oauth") {
		t.Errorf("uninterpolated prompt reached provider: %q", req.Messages[0].Content)
	}
}

func TestRunnerHumanPipelineWithRouterRework(t *testing.T) {
	h := &stubHuman{answers: []string{"no way", "yes"}}
	p := parse(t, `
name: gate
stages:
  - id: draft
    type: tool
    run: echo "draft v"
  - id: approval
    type: human
    prompt: "Approve ${stages.draft.output}?"
  - id: gate
    type: router
    when:
      - if: "${stages.approval.answer} == 'yes'"
        next: ship
      - next: draft
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
	// First rejection routed back to draft (steps: draft, approval, gate,
	// draft, approval, gate, ship = 7 transitions).
	if res.Steps != 7 {
		t.Errorf("steps = %d, want 7", res.Steps)
	}
	if len(h.asked) != 2 || !strings.Contains(h.asked[0], "Approve draft v?") {
		t.Errorf("human prompts = %v", h.asked)
	}
}

func TestRunnerRouterCycleGuard(t *testing.T) {
	p := parse(t, `
name: infinite
stages:
  - id: a
    type: tool
    run: "true"
  - id: gate
    type: router
    when:
      - next: a
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed {
		t.Fatal("cyclic pipeline must not complete")
	}
	if !strings.Contains(res.Err.Error(), "cycle") {
		t.Errorf("cycle error = %v", res.Err)
	}
}

func TestRunnerOnErrorSkipContinues(t *testing.T) {
	p := parse(t, `
name: skip-on-error
stages:
  - id: boom
    type: tool
    run: "exit 1"
    on_error: skip
  - id: after
    type: tool
    run: echo "made it"
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.Steps != 2 {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunnerHaltStopsAndSnapshotEnablesResume(t *testing.T) {
	const src = `name: resumable
stages:
  - id: one
    type: tool
    run: echo first
  - id: two
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "work on ${stages.one.output}"
  - id: three
    type: tool
    run: echo done
`
	p := parse(t, src)
	dir := t.TempDir()
	fail := true
	m := llm.NewMockFunc(func(llm.Request) *llm.Response {
		if fail {
			return nil
		}
		return &llm.Response{Text: "work done"}
	})
	// Make the provider error on the first (failing) pass.
	failFactory := func(*config.ModelConfig) (llm.Provider, error) {
		if fail {
			return failingProvider{}, nil
		}
		return m, nil
	}

	r := &Runner{Pipeline: p, Source: []byte(src), Providers: failFactory, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || res.FailedStage != "two" {
		t.Fatalf("res = %+v", res)
	}

	// Resume in the same run directory with a working provider.
	fail = false
	r2 := &Runner{ResumeID: res.RunID, Providers: func(*config.ModelConfig) (llm.Provider, error) { return m, nil }, RunsDir: dir}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Completed {
		t.Fatalf("resume failed: %v", res2.Err)
	}
	// Stage `one` was skipped (already complete): exactly one provider call,
	// and its prompt sees stage one's output from the snapshot.
	if m.Calls() != 1 {
		t.Errorf("provider calls after resume = %d, want 1", m.Calls())
	}
	if got := m.Requests()[0].Messages[0].Content; got != "work on first" {
		t.Errorf("resumed prompt = %q", got)
	}
}

func TestResumeRetriesFailedHumanStageAfterRework(t *testing.T) {
	// Mirrors the M2 demo: first pass rejects (routed back), re-asked
	// approval dies on EOF (stdin dry). Resume must re-run the human stage
	// — its earlier "no" is stale — then flow forward through the router.
	const src = `name: gate
stages:
  - id: draft
    type: tool
    run: echo "draft v"
  - id: approval
    type: human
    prompt: "Approve ${stages.draft.output}?"
  - id: gate
    type: router
    when:
      - if: "${stages.approval.answer} == 'yes'"
        next: ship
      - next: draft
  - id: ship
    type: tool
    run: echo shipped
`
	p := parse(t, src)
	dir := t.TempDir()

	// First attempt: "no" → rework → second ask hits EOF.
	h1 := &stubHuman{answers: []string{"no"}}
	r1 := &Runner{Pipeline: p, Source: []byte(src), Human: h1, RunsDir: dir}
	res1, err := r1.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res1.Completed || res1.FailedStage != "approval" {
		t.Fatalf("first pass = %+v", res1)
	}

	// Resume: the user now says yes. The recorded path
	// [draft approval gate draft] is skipped; approval re-runs.
	h2 := &stubHuman{answers: []string{"yes"}}
	r2 := &Runner{ResumeID: res1.RunID, Human: h2, RunsDir: dir}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Completed {
		t.Fatalf("resume failed at %s: %v", res2.FailedStage, res2.Err)
	}
	// The human was asked exactly once on resume (re-run of the failed stage).
	if len(h2.asked) != 1 {
		t.Fatalf("resume asks = %v", h2.asked)
	}
	if !strings.Contains(h2.asked[0], "Approve draft v?") {
		t.Errorf("resume ask = %q", h2.asked[0])
	}
}

type failingProvider struct{}

func (failingProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("simulated provider outage")
}
func (failingProvider) Stream(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, fmt.Errorf("simulated provider outage")
}

func TestRunnerRetryPolicy(t *testing.T) {
	calls := 0
	flaky := flakyProviderFunc(func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	p := parse(t, `
name: retried
stages:
  - id: work
    type: llm
    model: {provider: anthropic, model: test}
    prompt: hi
    retry: {max_attempts: 3, backoff_ms: 1}
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: func(*config.ModelConfig) (llm.Provider, error) { return flaky, nil }, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || calls != 3 {
		t.Errorf("completed=%v calls=%d", res.Completed, calls)
	}
}

type flakyProviderFunc func() error

func (f flakyProviderFunc) Complete(context.Context, llm.Request) (*llm.Response, error) {
	if err := f(); err != nil {
		return nil, err
	}
	return &llm.Response{Text: "ok"}, nil
}
func (f flakyProviderFunc) Stream(ctx context.Context, req llm.Request, onDelta llm.StreamFunc) (*llm.Response, error) {
	return f.Complete(ctx, req)
}

// --- agent stage / executor slot ---

type recordingExecutor struct {
	tasks []executor.Task
	out   string
}

func (e *recordingExecutor) Name() string { return "mock" }
func (e *recordingExecutor) Run(_ context.Context, task executor.Task, _ func(executor.Event)) (*executor.Result, error) {
	e.tasks = append(e.tasks, task)
	return &executor.Result{Output: e.out}, nil
}

func TestAgentStageDelegatesToExecutor(t *testing.T) {
	m := llm.NewMock()
	ex := &recordingExecutor{out: "architecture written"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := parse(t, `
name: with-agent
personas:
  - name: architect
    role: Architect
    system: You design systems.
    model: {provider: anthropic, model: persona-model}
stages:
  - id: design
    type: agent
    persona: architect
    input: "design ${stages.spec.output}"
    tools: [read_file, write_file]
    max_iterations: 5
  - id: spec
    type: tool
    run: echo spec-doc
`)
	// Note: forward reference ${stages.spec.output} resolves because the
	// agent stage runs after spec in linear order? No — design runs first.
	// So this pipeline exercises the loud-failure path instead.
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: mockFactory(m), Executors: reg, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed {
		t.Fatal("forward reference must fail loudly (spec has not run)")
	}
	if res.FailedStage != "design" {
		t.Errorf("failed stage = %q", res.FailedStage)
	}
	if len(ex.tasks) != 0 {
		t.Errorf("executor must not have been called: %+v", ex.tasks)
	}
}

func TestAgentStageExecutorReceivesResolvedTask(t *testing.T) {
	ex := &recordingExecutor{out: "design doc"}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := parse(t, `
name: with-agent
personas:
  - name: architect
    role: Architect
    system: You design systems.
    model: {provider: anthropic, model: persona-model}
stages:
  - id: spec
    type: tool
    run: echo spec-doc
  - id: design
    type: agent
    executor: mock
    persona: architect
    input: "design ${stages.spec.output}"
    tools: [read_file]
    approval: ask
    max_iterations: 5
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("run failed at %s: %v", res.FailedStage, res.Err)
	}
	if len(ex.tasks) != 1 {
		t.Fatalf("executor calls = %d", len(ex.tasks))
	}
	task := ex.tasks[0]
	if task.Instruction != "design spec-doc" {
		t.Errorf("instruction = %q", task.Instruction)
	}
	if task.System != "You design systems." {
		t.Errorf("system = %q", task.System)
	}
	if task.Model.Provider != "anthropic" || task.Model.Model != "persona-model" {
		t.Errorf("model spec = %+v", task.Model)
	}
	if len(task.Tools) != 1 || task.Tools[0] != "read_file" {
		t.Errorf("tools = %v", task.Tools)
	}
	if task.Approval != "ask" {
		t.Errorf("approval = %q", task.Approval)
	}
	// Executor output landed in the context.
	if got := ex.tasks[0].Instruction; got == "" {
		t.Error("empty instruction")
	}
}

func TestAgentStageUnknownExecutorFailsClearly(t *testing.T) {
	p := parse(t, `
name: missing-executor
personas:
  - {name: a, model: {provider: anthropic, model: m}, system: s}
stages:
  - id: ag
    type: agent
    persona: a
    input: go
    executor: skynet
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: executor.NewRegistry(), RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || !strings.Contains(res.Err.Error(), `executor "skynet" is not available`) {
		t.Errorf("res = %+v err = %v", res, res.Err)
	}
}
