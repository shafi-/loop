package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// A run's token ledger: stage (and gate) calls record into the meter,
// the result carries the totals, state.json persists them, and a
// resumed run accumulates onto its predecessor's numbers.
func TestRunnerRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`
name: usage-demo
stages:
  - id: a
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "hello"
  - id: gate
    type: human
    gate: true
    prompt: "approve?"
`)
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		return &llm.Response{Text: "ANSWER", Usage: llm.Usage{InputTokens: 100, OutputTokens: 20}}
	})
	// Session 1: the stage runs (1 call); the gate is answered with free
	// words (1 classification call).
	r := &Runner{
		Pipeline: parse(t, string(src)), Source: src, Providers: mockFactory(m),
		RunsDir: dir, RunID: "usage-run",
		Human: humanStub{answer: "make it better please"},
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Calls != 2 || res.Usage.InputTokens != 200 || res.Usage.OutputTokens != 40 {
		t.Fatalf("usage = %+v, want 2 calls/200/40", res.Usage)
	}
	// state.json carries the same totals for the next session to add onto.
	st, err := LoadState(dir, "usage-run")
	if err != nil || st == nil {
		t.Fatalf("state: %v %v", st, err)
	}
	if st.Usage == nil || st.Usage.Calls != 2 {
		t.Fatalf("state usage = %+v", st.Usage)
	}

	// Session 2 resumes a PAUSED run and its totals add onto session 1's.
	pauseSrc := []byte(`
name: pause-demo
stages:
  - id: a
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "hello"
  - id: gate
    type: human
    gate: true
    prompt: "approve?"
`)
	pause := parse(t, string(pauseSrc))
	rp := &Runner{
		Pipeline: pause, Source: pauseSrc, Providers: mockFactory(m),
		RunsDir: dir, RunID: "pause-run", Human: humanStub{err: ErrPaused},
	}
	if _, err := rp.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stp, _ := LoadState(dir, "pause-run")
	if stp.Usage == nil || stp.Usage.Calls != 1 {
		t.Fatalf("paused state usage = %+v, want the 1 stage call", stp.Usage)
	}
	r2 := &Runner{
		Pipeline: pause, Source: pauseSrc, Providers: mockFactory(m),
		RunsDir: dir, ResumeID: "pause-run", Human: humanStub{answer: "words, not crisp"},
	}
	res2, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Prior session's 1 call + this session's gate classification call.
	if res2.Usage.Calls != 2 || res2.Usage.InputTokens != 200 {
		t.Fatalf("resumed usage = %+v, want accumulated 2 calls/200 in", res2.Usage)
	}

	// The usage event lands in the log the UI timeline renders.
	data, err := os.ReadFile(filepath.Join(dir, "usage-run", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"usage"`) {
		t.Error("events.jsonl missing the usage event")
	}
	var found bool
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev struct {
			Type  string `json:"type"`
			Calls int    `json:"calls"`
		}
		if json.Unmarshal([]byte(ln), &ev) == nil && ev.Type == "usage" && ev.Calls > 0 {
			found = true
		}
	}
	if !found {
		t.Error("usage event payload malformed")
	}
}

// A run with no metered calls (all-tool pipeline) reports zeroes and
// writes no usage event — no noise.
func TestRunnerNoUsageForToolOnlyRuns(t *testing.T) {
	dir := t.TempDir()
	p := parse(t, `
name: tools-only
stages:
  - id: a
    type: tool
    run: 'echo hi'
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), RunsDir: dir, RunID: "tool-run"}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Calls != 0 {
		t.Fatalf("usage = %+v, want zero", res.Usage)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "tool-run", "events.jsonl"))
	if strings.Contains(string(data), `"usage"`) {
		t.Error("zero-call runs must not emit a usage event")
	}
}

// The narrator's provider wraps into the run's ledger too (wired by the
// CLI); the meter-level behavior is usage.Wrap's, verified in the usage
// package — here we check the runner accepts and reuses a pre-set meter.
func TestRunnerAcceptsPresetMeter(t *testing.T) {
	dir := t.TempDir()
	m := llm.NewMockFunc(func(llm.Request) *llm.Response {
		return &llm.Response{Text: "x", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}}
	})
	p := parse(t, `
name: preset
stages:
  - id: a
    type: llm
    model: {provider: anthropic, model: test}
    prompt: "hi"
`)
	meter := usage.NewMeter()
	r := &Runner{Pipeline: p, Source: []byte("x"), Providers: mockFactory(m), RunsDir: dir, Usage: meter}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tot := meter.Totals(); tot.Calls != 1 || tot.InputTokens != 7 {
		t.Fatalf("preset meter = %+v", tot)
	}
}

type humanStub struct {
	answer string
	err    error // returned instead of an answer (e.g. ErrPaused)
}

func (h humanStub) Prompt(_ context.Context, _ string) (string, error) {
	if h.err != nil {
		return "", h.err
	}
	return h.answer, nil
}
