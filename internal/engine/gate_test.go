package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// gatePipeline has the full three-arm gate contract: yes ships, no
// rejects explicitly, anything else revises. The two ending branches
// are terminal stages — no guard routers needed.
const gatePipeline = `name: gate
stages:
  - id: draft
    type: tool
    run: echo "draft v1"
  - id: approval
    type: human
    gate: true
    prompt: "Approve ${stages.draft.output}? Reply yes, no, or describe changes."
  - id: route
    type: router
    when:
      - if: "${stages.approval.intent} == 'yes'"
        next: ship
      - if: "${stages.approval.intent} == 'no'"
        next: rejected
      - next: revise
  - id: revise
    type: llm
    prompt: "Apply these changes: ${stages.approval.answer}"
  - id: back
    type: router
    when:
      - next: approval
  - id: ship
    type: tool
    terminal: true
    run: echo SHIPPED
  - id: rejected
    type: tool
    terminal: true
    run: echo REJECTED
`

func runGate(t *testing.T, answers []string, classifier *llm.Mock) (*RunResult, string, string) {
	t.Helper()
	p := parse(t, gatePipeline)
	h := &stubHuman{answers: answers}
	dir := t.TempDir()
	factory := func(*config.ModelConfig) (llm.Provider, error) { return classifier, nil }
	r := &Runner{Pipeline: p, Source: []byte(gatePipeline), Providers: factory, Human: h, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, dir, res.RunID)
	ctxSnap, err := os.ReadFile(filepath.Join(dir, res.RunID, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	return res, events, string(ctxSnap)
}

func TestGateVocabularyIsDeterministic(t *testing.T) {
	// Crisp yes/no never touches a provider — assert by handing the
	// factory a provider that fails the test if called.
	called := false
	p := parse(t, gatePipeline)
	h := &stubHuman{answers: []string{"YES"}}
	factory := func(*config.ModelConfig) (llm.Provider, error) {
		called = true
		return nil, errors.New("provider must not be needed for vocabulary answers")
	}
	r := &Runner{Pipeline: p, Source: []byte(gatePipeline), Providers: factory, Human: h, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("vocabulary answer must not trigger a provider call")
	}
	if !res.Completed {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	if h.asked[0] == "" || res.Steps == 0 {
		t.Errorf("unexpected run shape: %+v", res)
	}
}

func TestGateWordsGetOneClassificationCall(t *testing.T) {
	// Free words amounting to approval: one classifier call, intent yes.
	m := llm.NewMock(&llm.Response{Text: "yes", StopReason: llm.StopEndTurn})
	res, events, ctxSnap := runGate(t, []string{"looks good, ship it"}, m)
	if !res.Completed || res.FailedStage != "" {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	if m.Calls() != 1 {
		t.Errorf("classifier calls = %d, want exactly 1", m.Calls())
	}
	got := m.Requests()[0].Messages[0].Content
	if !strings.Contains(got, "Approve draft v1?") || !strings.Contains(got, "looks good, ship it") {
		t.Errorf("classifier must see the question and the words:\n%s", got)
	}
	if m.Requests()[0].System != gateClassifierSystem {
		t.Errorf("classifier system prompt not applied")
	}
	for _, want := range []string{`"intent":"yes"`, `"via":"llm"`} {
		if !strings.Contains(events, want) {
			t.Errorf("run log missing %q", want)
		}
	}
	if !strings.Contains(ctxSnap, "SHIPPED") {
		t.Errorf("approval intent should ship:\n%s", ctxSnap)
	}
}

func TestGateWordsRequestChanges(t *testing.T) {
	// Words that mean "change it": classifier says changes → revise →
	// approval again → vocabulary yes → ship.
	m := llm.NewMock(
		&llm.Response{Text: "changes", StopReason: llm.StopEndTurn},
		&llm.Response{Text: "REVISED", StopReason: llm.StopEndTurn},
	)
	res, events, _ := runGate(t, []string{"make the hero shorter", "y"}, m)
	if !res.Completed {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	// classifier + revise, and the second answer ("y") needed no call.
	if m.Calls() != 2 {
		t.Errorf("llm calls = %d, want 2 (classify, revise)", m.Calls())
	}
	if !strings.Contains(m.Requests()[1].Messages[0].Content, "make the hero shorter") {
		t.Errorf("revise should consume the raw words:\n%s", m.Requests()[1].Messages[0].Content)
	}
	if !strings.Contains(events, `"via":"vocabulary"`) {
		t.Errorf("second answer should classify via vocabulary:\n%s", events)
	}
}

func TestGateGarbageClassifierFallsBackToChanges(t *testing.T) {
	m := llm.NewMock(
		&llm.Response{Text: "sure thing boss, whatever you say", StopReason: llm.StopEndTurn}, // garbage
		&llm.Response{Text: "REVISED", StopReason: llm.StopEndTurn},
	)
	res, events, _ := runGate(t, []string{"make it pop", "yes"}, m)
	if !res.Completed {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	if !strings.Contains(events, `"intent":"changes"`) || !strings.Contains(events, `"via":"fallback"`) {
		t.Errorf("garbage classifier output must fall back to changes:\n%s", events)
	}
	// The fallback never approves: a revise round happened.
	if !strings.Contains(m.Requests()[1].Messages[0].Content, "make it pop") {
		t.Error("fallback should route words into revise")
	}
}

func TestGateRejectBranch(t *testing.T) {
	// A plain "no" routes to the explicit rejection arm, no LLM at all —
	// and the ship terminal must not run after it.
	m := llm.NewMock(&llm.Response{Text: "unused", StopReason: llm.StopEndTurn})
	res, events, ctxSnap := runGate(t, []string{"No"}, m)
	if !res.Completed {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	if m.Calls() != 0 {
		t.Errorf("reject must be provider-free, calls = %d", m.Calls())
	}
	if !strings.Contains(events, `"intent":"no"`) {
		t.Errorf("rejection intent not recorded:\n%s", events)
	}
	if !strings.Contains(ctxSnap, "REJECTED") || strings.Contains(ctxSnap, "SHIPPED") {
		t.Errorf("rejection arm must reject without shipping:\n%s", ctxSnap)
	}
}

func TestTerminalStageEndsRunWithoutFlowingOn(t *testing.T) {
	// A terminal ends the run where it is; linear successors must not
	// run, and the recorded done state forbids resume.
	const src = `name: terminals
stages:
  - id: gate
    type: router
    when:
      - if: "${vars.arm} == 'a'"
        next: alpha
      - next: beta
  - id: alpha
    type: tool
    terminal: true
    run: echo ALPHA-DONE
  - id: beta
    type: tool
    terminal: true
    run: echo BETA-DONE
`
	p := parse(t, src)
	p.Vars = map[string]any{"arm": "a"}
	dir := t.TempDir()
	r := &Runner{Pipeline: p, Source: []byte(src), RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("res = %+v (%v)", res, res.Err)
	}
	snap, err := os.ReadFile(filepath.Join(dir, res.RunID, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap), "ALPHA-DONE") || strings.Contains(string(snap), "BETA-DONE") {
		t.Errorf("terminal alpha must end the run before beta:\n%s", snap)
	}
	if st := readStateFile(t, dir, res.RunID); !strings.Contains(st, `"done": true`) {
		t.Errorf("state = %s", st)
	}

	// A completed run refuses resume.
	r2 := &Runner{ResumeID: res.RunID, RunsDir: dir}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Completed || res2.Steps != 0 {
		t.Errorf("resume of a terminal-ended run should be a no-op, got %+v", res2)
	}
}

func TestGateClassifierProviderErrorFailsWithHint(t *testing.T) {
	p := parse(t, gatePipeline)
	h := &stubHuman{answers: []string{"make it pop"}}
	factory := func(*config.ModelConfig) (llm.Provider, error) {
		return authFailingProvider{}, nil
	}
	r := &Runner{Pipeline: p, Source: []byte(gatePipeline), Providers: factory, Human: h, RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || res.FailedStage != "approval" {
		t.Fatalf("res = %+v, want failure at the gate", res)
	}
	if !strings.Contains(res.Err.Error(), "gate classification for approval") || !strings.Contains(res.Err.Error(), "hint:") {
		t.Errorf("gate failure should carry its hint: %v", res.Err)
	}
}
