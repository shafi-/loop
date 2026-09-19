package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/executor"
)

// taskRecording is a stand-in for the agent executor: it records the
// task it was given and reports the persona system prompt it received.
type taskRecording struct {
	tasks []executor.Task
}

func (e *taskRecording) Name() string { return "cline" }

func (e *taskRecording) Run(_ context.Context, task executor.Task, _ func(executor.Event)) (*executor.Result, error) {
	e.tasks = append(e.tasks, task)
	return &executor.Result{Output: "built"}, nil
}

// A pipeline authored with persona-library references (`persona: engineer`)
// must resume: the snapshot stores the reference as authored, and the
// resume path re-resolves it against the workspace's persona library —
// otherwise every agent stage dies with "persona not found in pipeline".
func TestResumeResolvesPersonaLibraryReferences(t *testing.T) {
	const src = `name: authored
personas:
  - persona: engineer
stages:
  - id: implement
    type: agent
    persona: engineer
    input: build it
`
	// newWS builds a workspace; withPersona seeds its project persona
	// library with an engineer.
	newWS := func(t *testing.T, withPersona bool) string {
		t.Helper()
		ws := t.TempDir()
		t.Setenv("HOME", t.TempDir()) // empty global library: the project dir decides
		if !withPersona {
			return ws
		}
		dir := filepath.Join(ws, ".loop/personas")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		const engineer = "name: engineer\nsystem: You build things.\n"
		if err := os.WriteFile(filepath.Join(dir, "engineer.yaml"), []byte(engineer), 0o644); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	newRegistry := func(rec *taskRecording) *executor.Registry {
		reg := executor.NewRegistry()
		reg.Register(rec)
		return reg
	}

	// halt runs a first pass with no executor registered: the agent stage
	// fails after the runlog (and its pipeline snapshot) are written.
	halt := func(t *testing.T, ws string) (string, *RunResult) {
		t.Helper()
		runsDir := t.TempDir()
		r := &Runner{Pipeline: parse(t, src), Source: []byte(src),
			Executors: executor.NewRegistry(), RunsDir: runsDir, CWD: ws}
		res, err := r.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Completed || res.FailedStage != "implement" {
			t.Fatalf("first pass = %+v, want a halt at implement", res)
		}
		return runsDir, res
	}

	t.Run("workspace persona library resolves", func(t *testing.T) {
		ws := newWS(t, true)
		runsDir, first := halt(t, ws)
		rec := &taskRecording{}
		r2 := &Runner{ResumeID: first.RunID, Executors: newRegistry(rec), RunsDir: runsDir, CWD: ws}
		res, err := r2.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !res.Completed {
			t.Fatalf("resume = %+v (err %v), want completion", res, res.Err)
		}
		if len(rec.tasks) != 1 {
			t.Fatalf("executor ran %d task(s), want 1", len(rec.tasks))
		}
		if got := rec.tasks[0].System; got != "You build things." {
			t.Errorf("executor system prompt = %q, want the library persona's", got)
		}
	})

	t.Run("missing persona fails the resume loudly", func(t *testing.T) {
		ws := newWS(t, false)
		runsDir, first := halt(t, ws)
		r2 := &Runner{ResumeID: first.RunID, Executors: newRegistry(&taskRecording{}), RunsDir: runsDir, CWD: ws}
		_, err := r2.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), `unknown persona "engineer"`) {
			t.Fatalf("err = %v, want the library lookup failure", err)
		}
	})
}
