package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The create page end to end: the editor's YAML validates against the
// daemon's strict parser, saves into the workspace's conventional
// directory, and an existing file is refused until the overwrite flag
// is set (the browser asks; here we assert the daemon side of that).
func TestCreatePageAndFlow(t *testing.T) {
	t.Chdir(t.TempDir())
	_, handler := newUI(t, ".")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// Both kinds render; unknown kinds 404.
	if resp, body := get(t, ts.URL+"/new/pipeline"); resp.StatusCode != 200 ||
		!strings.Contains(body, "draft with AI") || !strings.Contains(body, `value="pipeline"`) {
		t.Fatalf("pipeline page = %d %q", resp.StatusCode, body)
	}
	if resp, body := get(t, ts.URL+"/new/room"); resp.StatusCode != 200 || !strings.Contains(body, `value="room"`) {
		t.Fatalf("room page = %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, ts.URL+"/new/haiku"); resp.StatusCode != 404 {
		t.Errorf("unknown kind = %d, want 404", resp.StatusCode)
	}
	if resp, _ := get(t, ts.URL+"/new"); resp.StatusCode != 200 {
		t.Errorf("/new = %d, want 200 (redirect to a kind)", resp.StatusCode)
	}

	const goodPipeline = "name: drafted-pipe\nstages:\n  - id: a\n    type: llm\n    prompt: hello\n"

	// Validate reports every problem the CLI would.
	resp, body := postForm(t, ts.URL+"/new/validate",
		"kind=pipeline&yaml=name:+broken%0Astages:%0A++-+id:+a%0A++++type:+llm%0A")
	if resp.StatusCode != 200 || !strings.Contains(body, "problem(s)") || !strings.Contains(body, "stages[0].prompt") {
		t.Fatalf("invalid validate = %d %q", resp.StatusCode, body)
	}
	if resp, body = postForm(t, ts.URL+"/new/validate",
		"kind=pipeline&yaml="+urlQueryEscape(t, goodPipeline)); resp.StatusCode != 200 || !strings.Contains(body, "✓ valid") {
		t.Fatalf("valid validate = %d %q", resp.StatusCode, body)
	}

	// Save writes the file; saving again is a conflict until overwrite.
	resp, body = postForm(t, ts.URL+"/new/save",
		"kind=pipeline&overwrite=0&yaml="+urlQueryEscape(t, goodPipeline))
	if resp.StatusCode != 200 || !strings.Contains(body, "saved") {
		t.Fatalf("save = %d %q", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join("pipelines", "drafted-pipe.yaml")); err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if resp, _ = postForm(t, ts.URL+"/new/save",
		"kind=pipeline&overwrite=0&yaml="+urlQueryEscape(t, goodPipeline)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second save = %d, want 409", resp.StatusCode)
	}
	if resp, _ = postForm(t, ts.URL+"/new/save",
		"kind=pipeline&overwrite=1&yaml="+urlQueryEscape(t, goodPipeline)); resp.StatusCode != 200 {
		t.Fatalf("overwrite save = %d", resp.StatusCode)
	}

	// An invalid save is refused with the report, not written.
	resp, body = postForm(t, ts.URL+"/new/save",
		"kind=room&overwrite=0&yaml=" + urlQueryEscape(t, "name: broken\nagents: []\n"))
	if resp.StatusCode != 200 || !strings.Contains(body, "problem(s)") {
		t.Fatalf("invalid room save = %d %q", resp.StatusCode, body)
	}
}

func urlQueryEscape(t *testing.T, s string) string {
	t.Helper()
	return strings.ReplaceAll(strings.ReplaceAll(
		strings.ReplaceAll(s, "\n", "%0A"), " ", "+"), ":", "%3A")
}
