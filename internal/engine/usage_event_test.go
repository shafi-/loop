package engine

import (
	"context"
	"testing"

	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/llm"
)

// usageExecutor emits cumulative token totals like the cline host does:
// the executor owns the provider call, so its spend reaches loop only
// through these events.
type usageExecutor struct {
	steps [][2]int
}

func (e *usageExecutor) Name() string { return "mock" }
func (e *usageExecutor) Run(_ context.Context, _ executor.Task, onEvent func(executor.Event)) (*executor.Result, error) {
	for _, s := range e.steps {
		onEvent(executor.Event{Type: executor.EventUsage, InputTokens: s[0], OutputTokens: s[1]})
	}
	return &executor.Result{Output: "done"}, nil
}

// The run ledger records only the deltas between cumulative usage
// events, under the same stage:<id> label as provider-metered stages.
func TestAgentStageMetersExecutorUsage(t *testing.T) {
	ex := &usageExecutor{steps: [][2]int{{100, 50}, {150, 80}}}
	reg := executor.NewRegistry()
	reg.Register(ex)
	p := parse(t, `
name: metered-agent
personas:
  - name: architect
    role: Architect
    system: design it
stages:
  - id: design
    type: agent
    executor: mock
    persona: architect
    input: "do it"
`)
	r := &Runner{Pipeline: p, Source: []byte("x"), Executors: reg, Providers: mockFactory(llm.NewMock()), RunsDir: t.TempDir()}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Totals = last cumulative event, not the sum of events (150/80,
	// not 250/130).
	if res.Usage.InputTokens != 150 || res.Usage.OutputTokens != 80 {
		t.Errorf("usage = %+v", res.Usage)
	}
}
