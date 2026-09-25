package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// The activity tracker is raw truth: visible while set, idle after the
// turn's deferred clear, and keyed on What (never on Since).
func TestActivityTracker(t *testing.T) {
	r := &Room{}
	if _, ok := r.Activity(); ok {
		t.Fatal("a fresh room must be idle")
	}
	r.setActivity("eng", "running: echo hi")
	act, ok := r.Activity()
	if !ok || act.Agent != "eng" || act.What != "running: echo hi" {
		t.Fatalf("activity = %+v ok=%v", act, ok)
	}
	r.clearActivity()
	if _, ok := r.Activity(); ok {
		t.Fatal("cleared room must be idle")
	}
}

// probeProvider snapshots the room's activity from inside the model
// call — the only way to observe mid-turn state in a synchronous turn.
type probeProvider struct {
	room  *Room
	seen  []Activity
	calls int
}

func (p *probeProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	p.calls++
	act, ok := p.room.Activity()
	if ok {
		p.seen = append(p.seen, act)
	}
	if p.calls == 1 {
		return &llm.Response{
			ToolCalls:  []llm.ToolCall{{ID: "t1", Name: "read_file", Args: `{"path":"plan.md"}`}},
			StopReason: llm.StopToolUse,
		}, nil
	}
	return &llm.Response{Text: "done", StopReason: llm.StopEndTurn}, nil
}

func (p *probeProvider) Stream(ctx context.Context, req llm.Request, onDelta llm.StreamFunc) (*llm.Response, error) {
	return p.Complete(ctx, req)
}

// A full turn announces itself while it runs and is fully idle when it
// ends: room-thinking at entry, per-agent thinking and tool activity
// during, cleared by the deferred clear on every exit path.
func TestActivityLifecycleOverTurn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte("the plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := OpenTranscript(filepath.Join(dir, ".loop", "rooms", "a"))
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeProvider{}
	writer := &agent.Agent{
		Persona:  config.Persona{Name: "builder", Role: "Builder", Tools: []string{"read_file", "write_file"}},
		Provider: probe,
		CWD:      dir,
	}
	r := NewRoom(config.Room{Name: "a"}, []*agent.Agent{writer}, tr)
	probe.room = r
	r.setActivity("", "the room is thinking") // Say entry, replayed here

	var started [][2]string
	r.ToolStarted = func(agent, detail string) { started = append(started, [2]string{agent, detail}) }

	if err := r.Say(context.Background(), "@builder read the plan", &recorderUI{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Activity(); ok {
		t.Fatalf("activity survived the turn: %+v", r.act)
	}
	// Mid-turn: the provider saw the agent's thinking (and the tool's
	// running state exists only between probe calls, so the room-level
	// read inside the second call is the room-thinking handback).
	if len(probe.seen) == 0 || probe.seen[0].Agent != "builder" {
		t.Errorf("per-agent activity not announced mid-turn: %+v", probe.seen)
	}
	// The port heard the tool start (pre-execution, neutral verb).
	if len(started) != 1 || started[0][0] != "builder" || !strings.HasPrefix(started[0][1], "reading ") {
		t.Errorf("ToolStarted = %+v, want the builder's reading start", started)
	}
}

// A denied shell command never announces "running" — the start fires
// only after the approval gate, and the ask's "waiting for your
// answer" state is restored to thinking on denial.
func TestDeniedCommandNeverAnnouncesRunning(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(filepath.Join(dir, ".loop", "rooms", "d"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &agent.Agent{
		Persona:  config.Persona{Name: "runner", Role: "Runner", Tools: []string{"run_command"}},
		Provider: llm.NewMock(&llm.Response{Text: "fine", StopReason: llm.StopEndTurn}),
		CWD:      dir,
	}
	r := NewRoom(config.Room{Name: "d"}, []*agent.Agent{runner}, tr)
	r.ToolApproval = func(context.Context, string, string, string) (bool, string, error) {
		return false, "not that one", nil
	}
	var running []string
	r.ToolStarted = func(agent, detail string) {
		if strings.HasPrefix(detail, "running:") {
			running = append(running, detail)
		}
	}
	if err := r.Say(context.Background(), "@runner do the thing", &recorderUI{}); err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("a denied command announced itself as running: %v", running)
	}
	if _, ok := r.Activity(); ok {
		t.Fatalf("activity survived the turn")
	}
}
