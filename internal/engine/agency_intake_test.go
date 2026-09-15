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

// The flagship demo (examples/agency-intake) must never rot: these
// tests run the exact files a user would run. The room's transcript
// becomes pipeline input, a rejection loops through revise → approval,
// and the shipped file holds the revised plan.

const agencyIntakeDir = "../../examples/agency-intake"

// renamedExecutor re-exposes a recordingExecutor under the default
// executor name ("cline") so the shipped demo YAML runs unmodified.
type renamedExecutor struct {
	name  string
	inner *recordingExecutor
}

func (n *renamedExecutor) Name() string { return n.name }
func (n *renamedExecutor) Run(ctx context.Context, task executor.Task, cb func(executor.Event)) (*executor.Result, error) {
	return n.inner.Run(ctx, task, cb)
}

// loadAgencyPipeline reads and parses the demo pipeline as shipped.
func loadAgencyPipeline(t *testing.T) *config.Pipeline {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(agencyIntakeDir, "delivery-pipeline.yaml"))
	if err != nil {
		t.Fatalf("read demo pipeline: %v", err)
	}
	p, err := config.ParsePipeline(src)
	if err != nil {
		t.Fatalf("demo pipeline does not parse: %v", err)
	}
	return p
}

func TestAgencyIntakeRoomIsEnvDriven(t *testing.T) {
	room, err := config.LoadRoom(filepath.Join(agencyIntakeDir, "intake-room.yaml"))
	if err != nil {
		t.Fatalf("demo room does not load: %v", err)
	}
	// The transcript path the pipeline's intake stage depends on is
	// .loop/rooms/<name>/transcript.jsonl — the room name is contract.
	if room.Name != "agency-intake" {
		t.Errorf("room name = %q, want agency-intake (the pipeline's intake stage reads .loop/rooms/<name>/)", room.Name)
	}
	if len(room.Agents) != 3 {
		t.Fatalf("agents = %d, want 3 (lead, strategist, estimator)", len(room.Agents))
	}
	for _, a := range room.Agents {
		if a.Model != nil {
			t.Errorf("agent %q pins a model block; the demo is env-driven by design", a.Name)
		}
		if a.System == "" {
			t.Errorf("agent %q has no system prompt", a.Name)
		}
	}
}

func TestAgencyDeliveryPipelineRunsEndToEndWithRevisionLoop(t *testing.T) {
	p := loadAgencyPipeline(t)

	// Tool stages run in the process CWD; the demo's paths are
	// workspace-relative. Run inside a fake workspace with a settled
	// intake transcript, exactly what act one would have left behind.
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".loop/rooms/agency-intake", 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"ts":"2026-09-15T10:00:00Z","from":"user","text":"premium landing page for drone photography"}\n` +
		`{"ts":"2026-09-15T10:02:00Z","from":"lead","text":"deadline and budget?"}\n` +
		`{"ts":"2026-09-15T10:03:00Z","from":"user","text":"four weeks, modest budget"}\n`
	if err := os.WriteFile(".loop/rooms/agency-intake/transcript.jsonl", []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	// LLM stages in order: brief, the gate's classifier, then revise.
	// The revision must overwrite plan_md so approval and ship see the
	// latest.
	m := llm.NewMock(
		&llm.Response{Text: "BRIEF: premium drone photography landing page, four weeks, modest budget", StopReason: llm.StopEndTurn},
		&llm.Response{Text: "changes", StopReason: llm.StopEndTurn},
		&llm.Response{Text: "PLAN v2 — revised: shorter hero, budget kept", StopReason: llm.StopEndTurn},
	)
	// The demo's agent stage uses the default executor ("cline"); a
	// mock registered under that name lets the shipped YAML run as-is.
	ex := &recordingExecutor{out: "PLAN v1 — hero video, three workstreams"}
	reg := executor.NewRegistry()
	reg.Register(&renamedExecutor{name: "cline", inner: ex})

	h := &stubHuman{answers: []string{"make the hero shorter", "yes"}}
	r := &Runner{
		Pipeline:  p,
		Source:    []byte("demo"),
		Providers: mockFactory(m),
		Executors: reg,
		Human:     h,
		RunsDir:   filepath.Join(".loop", "runs"),
	}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("demo run failed at %s: %v", res.FailedStage, res.Err)
	}

	// The room transcript reached the brief prompt verbatim.
	if m.Calls() != 3 {
		t.Fatalf("llm calls = %d, want 3 (brief, gate classifier, revise)", m.Calls())
	}
	if got := m.Requests()[0].Messages[0].Content; !strings.Contains(got, "premium landing page for drone photography") {
		t.Errorf("brief prompt did not include the transcript:\n%s", got)
	}
	// The classifier saw the client's words against the plan under review.
	if got := m.Requests()[1].Messages[0].Content; !strings.Contains(got, "make the hero shorter") || !strings.Contains(got, "PLAN v1") {
		t.Errorf("classifier prompt missing the words or the plan:\n%s", got)
	}
	// The revision prompt carried the client's words and the current plan.
	if got := m.Requests()[2].Messages[0].Content; !strings.Contains(got, "make the hero shorter") || !strings.Contains(got, "PLAN v1") {
		t.Errorf("revise prompt missing client words or current plan:\n%s", got)
	}
	// The planner agent worked from the brief.
	if len(ex.tasks) != 1 || !strings.Contains(ex.tasks[0].Instruction, "BRIEF: premium drone") {
		t.Errorf("planner instruction = %+v", ex.tasks)
	}
	// The client reviewed twice: rejection, then approval.
	if len(h.asked) != 2 {
		t.Fatalf("approval prompts = %d, want 2 (revision loop)", len(h.asked))
	}
	if !strings.Contains(h.asked[1], "PLAN v2") {
		t.Errorf("second review should show the revised plan:\n%s", h.asked[1])
	}
	// The shipped file holds the revised plan — proof the loop's
	// context overwrite reached the deterministic ending.
	shipped, err := os.ReadFile(filepath.Join("deliveries", "plan.md"))
	if err != nil {
		t.Fatalf("ship stage wrote nothing: %v", err)
	}
	if !strings.Contains(string(shipped), "PLAN v2") {
		t.Errorf("deliveries/plan.md = %q, want the revised plan", shipped)
	}
}

func TestAgencyDeliveryPipelineIntakeStageFailsWithInstructions(t *testing.T) {
	p := loadAgencyPipeline(t)
	// A workspace with no room transcript: the first stage must fail
	// with instructions, not mystery.
	t.Chdir(t.TempDir())
	r := &Runner{Pipeline: p, Source: []byte("demo"), RunsDir: filepath.Join(".loop", "runs")}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || res.FailedStage != "intake" {
		t.Fatalf("res = %+v, want failure at intake", res)
	}
	if !strings.Contains(res.Err.Error(), "loop chat examples/agency-intake/intake-room.yaml") {
		t.Errorf("intake failure should point at the room command: %v", res.Err)
	}
}
