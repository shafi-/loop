package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/daemon"
)

// The room sidecar must offer a stop control for live runs — the most
// common way a pipeline starts is /run from a room, and without this
// the web UI had no way to stop one. Finished runs get no button.
func TestSidecarRendersStopForAliveRuns(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{
		Room: daemon.RoomInfo{Name: "dev"},
		Sidecar: []SidecarRun{
			{Info: daemon.RunInfo{RunID: "r-live", Alias: "implement", Phase: "running", Alive: true}},
			{Info: daemon.RunInfo{RunID: "r-done", Alias: "review", Phase: "done"}},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "sidecar_fragment", data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `/rooms/dev/runs/implement/halt`) {
		t.Errorf("live run has no stop button:\n%s", out)
	}
	if !strings.Contains(out, `hx-target="#sidecar"`) {
		t.Errorf("stop button does not refresh the sidecar:\n%s", out)
	}
	if strings.Contains(out, `/rooms/dev/runs/review/halt`) {
		t.Errorf("finished run got a stop button:\n%s", out)
	}
	// The button must not toggle the details when clicked.
	if !strings.Contains(out, "stopPropagation") {
		t.Errorf("stop button does not suppress the summary toggle:\n%s", out)
	}
}

// The dashboard's active table gets the same control: it halts through
// the run id and refreshes the runs fragment in place.
func TestFragRunsActiveRowHasStop(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{
		Active: []daemon.RunInfo{
			{RunID: "r1", Pipeline: "implement", Phase: "running", Alive: true},
		},
		History: []daemon.RunInfo{
			{RunID: "r2", Pipeline: "review", Phase: "done"},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "frag_runs", data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `hx-post="/runs/r1/halt"`) || !strings.Contains(out, `hx-target="#runs"`) {
		t.Errorf("active run row has no stop button:\n%s", out)
	}
	if strings.Contains(out, `/runs/r2/halt`) {
		t.Errorf("history got a stop button:\n%s", out)
	}
}
