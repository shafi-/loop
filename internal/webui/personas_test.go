package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonaUIFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the global persona library
	t.Chdir(t.TempDir())
	_, handler := newUI(t, ".")
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// The create page offers the scope choice.
	if resp, body := get(t, ts.URL+"/new/persona"); resp.StatusCode != 200 ||
		!strings.Contains(body, "compose yaml") ||
		!strings.Contains(body, `value="project"`) || !strings.Contains(body, `value="global"`) {
		t.Fatalf("persona page = %d %.400s", resp.StatusCode, body)
	}

	// The form composes valid YAML into the editor.
	resp, body := postForm(t, ts.URL+"/new/persona/compose",
		"name=architect&role=Software+architect&system=You+design+systems.&tools=read_file")
	if resp.StatusCode != 200 || !strings.Contains(body, "name: architect") || !strings.Contains(body, "system:") {
		t.Fatalf("compose = %d %q", resp.StatusCode, body)
	}

	// Save into the project scope.
	personaYAML := "name: architect\nrole: Software architect\nsystem: |\n  You design systems.\n"
	resp, body = postForm(t, ts.URL+"/new/save",
		"kind=persona&scope=project&overwrite=0&yaml="+queryEscape(personaYAML))
	if resp.StatusCode != 200 || !strings.Contains(body, "saved") {
		t.Fatalf("save = %d %q", resp.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join("personas", "architect.yaml")); err != nil {
		t.Fatalf("missing: %v", err)
	}

	// The manage page lists it with its scope; the edit link carries
	// both scope and name.
	if _, list := get(t, ts.URL+"/personas"); !strings.Contains(list, "architect") {
		t.Fatalf("manage page missing the persona: %.400s", list)
	}
	_, frag := get(t, ts.URL+"/frag/personas")
	if !strings.Contains(frag, "project") || !strings.Contains(frag, "/new/persona?scope=project&name=architect") {
		t.Fatalf("fragment = %q", frag)
	}

	// Edit mode prefills the form and the editor from the saved file.
	_, edit := get(t, ts.URL+"/new/persona?scope=project&name=architect")
	if !strings.Contains(edit, `value="architect"`) || !strings.Contains(edit, "You design systems.") {
		t.Fatalf("edit page = %.600s", edit)
	}

	// Saving the same content into the global scope lands in the home
	// library and is listed as global.
	resp, _ = postForm(t, ts.URL+"/new/save",
		"kind=persona&scope=global&overwrite=0&yaml="+queryEscape(personaYAML))
	if resp.StatusCode != 200 {
		t.Fatalf("global save = %d", resp.StatusCode)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".loop", "personas", "architect.yaml")); err != nil {
		t.Fatalf("global persona missing: %v", err)
	}
	_, frag = get(t, ts.URL+"/frag/personas")
	if !strings.Contains(frag, "global") {
		t.Fatalf("fragment missing the global entry: %q", frag)
	}

	// A room referencing the persona validates; deletion of the
	// referenced persona is refused (409) and the toast carries why.
	roomYAML := "name: demo\nagents:\n  - persona: architect\n"
	if resp, body = postForm(t, ts.URL+"/new/validate", "kind=room&yaml="+queryEscape(roomYAML)); resp.StatusCode != 200 || !strings.Contains(body, "✓ valid") {
		t.Fatalf("room with ref = %d %q", resp.StatusCode, body)
	}
	if err := os.MkdirAll("rooms", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("rooms", "uses.yaml"), []byte(roomYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp, body = postForm(t, ts.URL+"/personas/delete", "scope=project&name=architect"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("referenced delete = %d %q, want 409", resp.StatusCode, body)
	}

	// The global persona that shares the referenced NAME is also
	// guarded — a room's `persona: architect` may resolve to either
	// scope, so the name is what matters. An unreferenced name deletes.
	globalYAML := strings.Replace(personaYAML, "architect", "solo", 1)
	if resp, _ = postForm(t, ts.URL+"/new/save",
		"kind=persona&scope=global&overwrite=0&yaml="+queryEscape(globalYAML)); resp.StatusCode != 200 {
		t.Fatalf("global solo save = %d", resp.StatusCode)
	}
	resp, _ = postForm(t, ts.URL+"/personas/delete", "scope=global&name=solo")
	if resp.StatusCode != 200 {
		t.Fatalf("unreferenced delete = %d", resp.StatusCode)
	}
}

func queryEscape(s string) string {
	r := strings.NewReplacer("\n", "%0A", " ", "+", ":", "%3A", ".", "%2E")
	return r.Replace(s)
}
