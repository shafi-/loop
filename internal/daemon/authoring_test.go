package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/generator"
	"github.com/shafi-/loop/internal/llm"
)

const testPipelineYAML = `name: my-pipe
stages:
  - id: a
    type: llm
    prompt: say hello
`

const testRoomYAML = `name: my-room
agents:
  - name: a
    system: You are A.
`

// postJSON is the one-liner the authoring tests speak through.
func postJSON(t *testing.T, ts *httptest.Server, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func authoringServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Chdir(t.TempDir())
	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSaveEndpoint(t *testing.T) {
	ts := authoringServer(t)

	resp, out := postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "pipeline", "content": testPipelineYAML})
	if resp.StatusCode != 200 || out["path"] != filepath.Join("pipelines", "my-pipe.yaml") {
		t.Fatalf("save = %d %v", resp.StatusCode, out)
	}
	onDisk, err := os.ReadFile(filepath.Join("pipelines", "my-pipe.yaml"))
	if err != nil || string(onDisk) != testPipelineYAML {
		t.Fatalf("saved file = %q, %v", onDisk, err)
	}

	// Exists: refused unless the client confirms the overwrite.
	resp, _ = postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "pipeline", "content": testPipelineYAML})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second save = %d, want 409", resp.StatusCode)
	}
	resp, _ = postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "pipeline", "content": testPipelineYAML, "overwrite": true})
	if resp.StatusCode != 200 {
		t.Fatalf("overwrite save = %d", resp.StatusCode)
	}
}

func TestSaveDerivesFilenameFromDocument(t *testing.T) {
	ts := authoringServer(t)

	// The client's suggestion is ignored; the name comes from the YAML.
	content := strings.Replace(testPipelineYAML, "my-pipe", "My Great Pipe!!", 1)
	resp, out := postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "pipeline", "content": content})
	if resp.StatusCode != 200 || out["path"] != filepath.Join("pipelines", "my-great-pipe.yaml") {
		t.Fatalf("sanitized save = %d %v", resp.StatusCode, out)
	}

	// A room saves into rooms/.
	resp, out = postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "room", "content": testRoomYAML})
	if resp.StatusCode != 200 || out["path"] != filepath.Join("rooms", "my-room.yaml") {
		t.Fatalf("room save = %d %v", resp.StatusCode, out)
	}
}

func TestSaveRejectsInvalidContent(t *testing.T) {
	ts := authoringServer(t)
	resp, out := postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "pipeline", "content": "name: broken\nstages:\n  - id: a\n    type: llm\n"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid save = %d, want 422", resp.StatusCode)
	}
	if _, ok := out["errors"]; !ok {
		t.Fatalf("422 should carry the error list: %v", out)
	}
}

const testPersonaYAML = `name: architect
role: Software architect
system: |
  You design systems and name trade-offs.
`

func TestSavePersonaScope(t *testing.T) {
	t.Setenv("LOOP_TEST_HOME", t.TempDir()) // unused; GlobalPersonaDir uses UserHomeDir
	dir := t.TempDir()
	t.Chdir(dir)
	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Project scope lands in the workspace's personas/.
	resp, out := postJSON(t, ts, "/api/workspace/save",
		map[string]any{"kind": "persona", "scope": "project", "content": testPersonaYAML})
	if resp.StatusCode != 200 || out["path"] != filepath.Join("personas", "architect.yaml") {
		t.Fatalf("project persona save = %d %v", resp.StatusCode, out)
	}
	if _, err := os.Stat(filepath.Join("personas", "architect.yaml")); err != nil {
		t.Fatalf("missing: %v", err)
	}
}

func TestPersonasListDetailDelete(t *testing.T) {
	// The global library lives at $HOME/.loop/personas — point HOME at a
	// temp dir so the test stays hermetic (os.UserHomeDir reads $HOME).
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	save := func(scope, content string) {
		t.Helper()
		resp, out := postJSON(t, ts, "/api/workspace/save",
			map[string]any{"kind": "persona", "scope": scope, "content": content})
		if resp.StatusCode != 200 {
			t.Fatalf("save %s = %d %v", scope, resp.StatusCode, out)
		}
	}
	save("project", testPersonaYAML)

	// A global persona, written directly into the redirected home.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	globalDir := filepath.Join(home, ".loop", "personas")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalYAML := strings.Replace(testPersonaYAML, "architect", "reviewer", 1)
	if err := os.WriteFile(filepath.Join(globalDir, "reviewer.yaml"), []byte(globalYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// List shows both scopes, project first.
	listResp, err := http.Get(ts.URL + "/api/personas")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list struct {
		Personas []struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		} `json:"personas"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Personas) != 2 || list.Personas[0].Name != "architect" || list.Personas[0].Scope != "project" ||
		list.Personas[1].Name != "reviewer" || list.Personas[1].Scope != "global" {
		t.Fatalf("personas = %+v", list.Personas)
	}

	// Detail returns the file's YAML.
	detailResp, err := http.Get(ts.URL + "/api/personas/detail?scope=global&name=reviewer")
	if err != nil {
		t.Fatal(err)
	}
	defer detailResp.Body.Close()
	var detail struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.YAML, "name: reviewer") {
		t.Fatalf("detail yaml = %q", detail.YAML)
	}

	// A room referencing the persona blocks its deletion...
	room := filepath.Join("rooms", "uses-architect.yaml")
	if err := os.MkdirAll("rooms", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(room, []byte("name: uses-architect\nagents:\n  - persona: architect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	delResp, out := postJSON(t, ts, "/api/personas/delete",
		map[string]any{"scope": "project", "name": "architect"})
	if delResp.StatusCode != http.StatusConflict {
		t.Fatalf("referenced delete = %d %v, want 409", delResp.StatusCode, out)
	}
	// ...the unreferenced one deletes fine.
	delResp, _ = postJSON(t, ts, "/api/personas/delete",
		map[string]any{"scope": "global", "name": "reviewer"})
	if delResp.StatusCode != 200 {
		t.Fatalf("unreferenced delete = %d", delResp.StatusCode)
	}
}

func TestValidateRoomChecksPersonaReferences(t *testing.T) {
	ts := authoringServer(t)
	// A room referencing a persona that exists in the project library
	// validates; a missing one fails at resolution.
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("personas", "architect.yaml"), testPersonaYAML)

	room := "name: demo\nagents:\n  - persona: architect\n"
	resp, out := postJSON(t, ts, "/api/workspace/validate", map[string]any{"kind": "room", "content": room})
	if resp.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("valid ref room = %d %v", resp.StatusCode, out)
	}
	room = strings.Replace(room, "architect", "ghost", 1)
	resp, out = postJSON(t, ts, "/api/workspace/validate", map[string]any{"kind": "room", "content": room})
	if resp.StatusCode != 200 || out["ok"] != false {
		t.Fatalf("unknown ref room = %d %v", resp.StatusCode, out)
	}
}

func TestValidateEndpoint(t *testing.T) {
	ts := authoringServer(t)

	resp, out := postJSON(t, ts, "/api/workspace/validate",
		map[string]any{"kind": "pipeline", "content": testPipelineYAML})
	if resp.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("valid = %d %v", resp.StatusCode, out)
	}

	resp, out = postJSON(t, ts, "/api/workspace/validate",
		map[string]any{"kind": "pipeline", "content": "name: broken\nstages:\n  - id: a\n    type: llm\n"})
	if resp.StatusCode != 200 || out["ok"] != false {
		t.Fatalf("invalid = %d %v", resp.StatusCode, out)
	}
	errs := out["errors"].([]any)
	first := errs[0].(map[string]any)
	if first["path"] == "" || first["message"] == "" {
		t.Errorf("issues should carry path and message: %v", first)
	}

	// Bad kind is a client error; empty content is a normal failure.
	resp, _ = postJSON(t, ts, "/api/workspace/validate", map[string]any{"kind": "haiku", "content": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind = %d, want 400", resp.StatusCode)
	}
	resp, out = postJSON(t, ts, "/api/workspace/validate", map[string]any{"kind": "room", "content": "  "})
	if resp.StatusCode != 200 || out["ok"] != false {
		t.Fatalf("empty = %d %v", resp.StatusCode, out)
	}
}

// fakeGenerator swaps the env-driven provider for a canned one.
func fakeGenerator(t *testing.T, resp *llm.Response) func() (*generator.Generator, error) {
	return func() (*generator.Generator, error) {
		return &generator.Generator{Provider: llm.NewMock(resp), Model: "test-model"}, nil
	}
}

func TestDraftEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	srv.newGenerator = fakeGenerator(t, &llm.Response{
		Text: `{"name": "drafted", "stages": [
			{"id": "a", "type": "llm", "prompt": "hello"}]}`,
		StopReason: llm.StopEndTurn,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, out := postJSON(t, ts, "/api/workspace/draft",
		map[string]any{"kind": "pipeline", "description": "say hello"})
	if resp.StatusCode != 200 {
		t.Fatalf("draft = %d %v", resp.StatusCode, out)
	}
	yaml, _ := out["yaml"].(string)
	if !strings.Contains(yaml, "name: drafted") {
		t.Errorf("drafted yaml = %q", yaml)
	}

	// Rooms go through the same endpoint with their own gate.
	srv2 := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	srv2.newGenerator = fakeGenerator(t, &llm.Response{
		Text: `{"name": "war-room", "agents": [{"name": "scout", "system": "You scout."}]}`,
		StopReason: llm.StopEndTurn,
	})
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()
	resp, out = postJSON(t, ts2, "/api/workspace/draft",
		map[string]any{"kind": "room", "description": "a scouting team"})
	if resp.StatusCode != 200 || !strings.Contains(out["yaml"].(string), "name: war-room") {
		t.Fatalf("room draft = %d %v", resp.StatusCode, out)
	}
}

func TestDraftReportsFailuresHonesty(t *testing.T) {
	// No provider configured: a 503 that names what's missing. Keys are
	// pinned empty so the assertion holds on any developer machine.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_BASE_URL", "")
	ts := authoringServer(t)
	resp, out := postJSON(t, ts, "/api/workspace/draft",
		map[string]any{"kind": "pipeline", "description": "x"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("provider-less draft = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(out["error"].(string), "provider") {
		t.Errorf("503 should name the provider problem: %v", out)
	}

	// Provider answers garbage past the repair budget: a 502, never a guess.
	dir := t.TempDir()
	t.Chdir(dir)
	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	srv.newGenerator = fakeGenerator(t, &llm.Response{Text: "no json here", StopReason: llm.StopEndTurn})
	ts2 := httptest.NewServer(srv.Handler())
	defer ts2.Close()
	resp, out = postJSON(t, ts2, "/api/workspace/draft",
		map[string]any{"kind": "pipeline", "description": "x"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("garbage draft = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(out["error"].(string), "valid pipeline") {
		t.Errorf("502 should carry the generator's honest failure: %v", out)
	}
}
