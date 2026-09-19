package roomkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"gopkg.in/yaml.v3"
)

// The generated pipeline must parse as a real pipeline — a regression
// in tags or escaping fails here, deterministically.
func TestBuildDivePipelineRoundTrips(t *testing.T) {
	spec := chat.DiveSpec{
		Agent:       "architect",
		Role:        "Architect",
		Topic:       "session invalidation in auth/",
		Question:    "why do sessions leak?",
		Tools:       []string{"read_file", "write_file", "run_command", "project_notes"},
		System:      "You are the architect. Multi\nline \"system\" prompt with: colons",
		ContextTail: "[ceo] why do sessions leak?\n[architect] My read: they leak.",
	}
	data, err := yaml.Marshal(buildDivePipeline(spec))
	if err != nil {
		t.Fatal(err)
	}
	p, err := config.ParsePipeline(data)
	if err != nil {
		t.Fatalf("generated pipeline does not parse: %v\n%s", err, data)
	}
	if p.Name != "deep-dive-architect" || len(p.Stages) != 1 {
		t.Fatalf("pipeline = %+v", p)
	}
	st := p.Stages[0]
	if st.ID != "dive" || st.Agent.Executor != "cline" || st.Agent.Approval != "ask" {
		t.Errorf("stage = %+v", st)
	}
	// project_notes is room-native — the host must never see it.
	for _, tool := range st.Agent.Tools {
		if tool == "project_notes" {
			t.Error("project_notes leaked into the dive pipeline")
		}
	}
	if st.Agent.MaxIterations != 12 || st.Agent.Output != "dive" {
		t.Errorf("stage budget/output = %+v", st.Agent)
	}
}

// The completion read-back: a completed dive run's context.json lands
// in the transcript as the agent's report line, run id attached.
func TestDiveCoordinatorLandsReport(t *testing.T) {
	dir := t.TempDir()
	tr, err := chat.OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &DiveCoordinator{
		roomDir: dir, runsDir: filepath.Join(dir, "runs"), tr: tr,
		metas: map[string]diveMeta{},
	}
	// A fake completed run: stage output in context.json, the shape
	// engine.ReadStageOutput parses.
	runID := "20260920-120000-abcd"
	runDir := filepath.Join(c.runsDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := `{"vars":{},"stages":{},"outputs":{"dive":"Sessions leak in auth/session.go — verified by reading the store."}}`
	if err := os.WriteFile(filepath.Join(runDir, "context.json"), []byte(snap), 0o644); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.metas["dive-architect"] = diveMeta{Agent: "architect", Topic: "session invalidation"}
	c.mu.Unlock()

	if !c.Handles("dive-architect") || c.Handles("implement") {
		t.Fatal("alias claiming broken")
	}
	c.OnEnded("dive-architect", runID, "completed")
	line := tr.Messages[len(tr.Messages)-1].Text
	if !strings.HasPrefix(line, "@architect (deep dive):") || !strings.Contains(line, runID) {
		t.Errorf("report line = %q", line)
	}

	// A failed run lands nothing: the transcript is unchanged.
	c.mu.Lock()
	c.metas["dive-architect"] = diveMeta{Agent: "architect", Topic: "x"}
	c.mu.Unlock()
	before := len(tr.Messages)
	c.OnEnded("dive-architect", runID, "failed")
	if len(tr.Messages) != before {
		t.Errorf("failed run appended a line: %q", tr.Messages[len(tr.Messages)-1].Text)
	}
}

// AttachDives with no launcher (no run session) leaves the room unable
// to dive, with the reason /deepdive will report. With one, StartDive
// launches through the injected start — provided the executor gate
// passes on this machine (the launch path is otherwise covered by the
// chat-level stub-hook tests).
func TestAttachDivesWiring(t *testing.T) {
	dir := t.TempDir()
	tr, err := chat.OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	room := chat.NewRoom(config.Room{Name: "dev"}, nil, tr)
	c := AttachDives(room, tr, dir, "", nil, nil, nil)
	if c != nil || room.StartDive != nil {
		t.Fatal("a room without a run session must not dive")
	}
	if room.DiveUnavailableReason == "" {
		t.Error("expected a reason for /deepdive to report")
	}

	started := ""
	room2 := chat.NewRoom(config.Room{Name: "dev"}, nil, tr)
	c2 := AttachDives(room2, tr, dir, "", func(alias, file string) error {
		started = alias
		return nil
	}, nil, nil)
	if c2 == nil {
		t.Skip("executor not ready on this machine — the launch path is covered by the stub-hook tests")
	}
	if err := room2.StartDive(context.Background(), chat.DiveSpec{Agent: "architect", Topic: "t"}); err != nil {
		t.Fatal(err)
	}
	if started != "dive-architect" {
		t.Errorf("start alias = %q", started)
	}
	if _, err := os.Stat(filepath.Join(dir, "dives")); err != nil {
		t.Errorf("dive pipeline file not kept: %v", err)
	}
}
