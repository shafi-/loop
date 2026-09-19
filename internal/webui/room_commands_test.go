package webui

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/daemon"
)

// The room page must render the composer's command palette data from
// the daemon's verb list — the "/" menu is built from it client-side.
func TestRoomPageRendersCommandPaletteData(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{
		Title:     "demo",
		Workspace: "demo",
		FullBleed: true,
		Room: daemon.RoomInfo{
			Name: "demo",
			Commands: []chat.CommandInfo{
				{Name: "cost", Desc: "session ledger"},
				{Name: "notes", Desc: "project knowledge"},
			},
			Runnable: []daemon.PipelineOption{
				{Name: "implement", File: "../pipelines/implement.yaml"},
				{Name: "pipelines/review.yaml", File: "pipelines/review.yaml"},
			},
		},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "room", data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `data-commands=`) {
		t.Fatal("composer missing the data-commands attribute")
	}
	if !strings.Contains(out, "session ledger") || !strings.Contains(out, "project knowledge") {
		t.Errorf("verb descriptions not rendered into the palette data")
	}
	if !strings.Contains(out, "/ for commands") {
		t.Error("composer placeholder should advertise the palette")
	}
	// The /run second level reads data-pipelines; the sidecar dropdown
	// offers the same list.
	if !strings.Contains(out, `data-pipelines=`) || !strings.Contains(out, "pipelines/review.yaml") {
		t.Error("runnable pipelines not rendered into the composer/sidecar")
	}
	if !strings.Contains(out, `<option value="implement">implement</option>`) {
		t.Error("sidecar dropdown should list runnable pipelines")
	}
}
