package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const architectYAML = `name: architect
role: Software architect
system: |
  You design systems and name trade-offs.
tools: [read_file]
`

const reviewerYAML = `name: reviewer
role: Reviewer
system: |
  You review everything twice.
`

func writePersona(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsePersona(t *testing.T) {
	p, err := ParsePersona([]byte(architectYAML))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "architect" || p.System == "" || len(p.Tools) != 1 {
		t.Fatalf("persona = %+v", p)
	}

	// Unknown fields are errors, as everywhere in loop.
	if _, err := ParsePersona([]byte("name: x\nsystem: s\nmodle: typo\n")); err == nil {
		t.Error("unknown field should be a hard error")
	}
	// A persona file is concrete: no references inside the library.
	_, err = ParsePersona([]byte("name: x\nsystem: s\npersona: y\n"))
	if err == nil || !strings.Contains(err.Error(), "persona:") {
		t.Errorf("ref inside library = %v, want a persona: error", err)
	}
}

func TestPersonaLibraryPrecedence(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "personas")
	global := filepath.Join(dir, "global-personas")
	writePersona(t, project, "architect.yaml", architectYAML)
	writePersona(t, global, "architect.yaml",
		strings.Replace(architectYAML, "You design systems", "GLOBAL You design systems", 1))
	writePersona(t, global, "reviewer.yaml", reviewerYAML)

	lib := NewPersonaLibrary(project, global)

	// Project shadows global for the same name.
	p, err := lib.Lookup("architect")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.System, "You design systems") || strings.Contains(p.System, "GLOBAL") {
		t.Errorf("project persona should win: %q", p.System)
	}
	// The global-only persona resolves too.
	if _, err := lib.Lookup("reviewer"); err != nil {
		t.Fatalf("global-only persona: %v", err)
	}
	// Unknown names name the places they looked.
	_, err = lib.Lookup("ghost")
	if err == nil || !strings.Contains(err.Error(), "looked in") {
		t.Errorf("unknown persona = %v", err)
	}
}

func TestPersonaLibraryBrokenFile(t *testing.T) {
	dir := t.TempDir()
	personas := filepath.Join(dir, "personas")
	writePersona(t, personas, "broken.yaml", "name: broken\nrole: no system prompt\n")

	lib := NewPersonaLibrary(personas)
	// Referencing the broken file reports the parse problem, not just
	// "unknown".
	_, err := lib.Lookup("broken")
	if err == nil || !strings.Contains(err.Error(), "not a valid persona file") {
		t.Fatalf("broken lookup = %v", err)
	}
	// It doesn't stop other names from resolving.
	if names := lib.Names(); len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}
}

func TestRoomResolvePersonas(t *testing.T) {
	dir := t.TempDir()
	personas := filepath.Join(dir, "personas")
	writePersona(t, personas, "architect.yaml", architectYAML)

	roomYAML := `name: demo
agents:
  - persona: architect
  - persona: architect
    tools: [read_file, write_file]
    model:
      provider: openai
  - name: inline
    role: Inline
    system: |
      You are defined inline.
`
	r, err := ParseRoom([]byte(roomYAML))
	if err != nil {
		t.Fatal(err)
	}
	lib := NewPersonaLibrary(personas)
	if err := r.ResolvePersonas(lib); err != nil {
		t.Fatal(err)
	}
	if r.Agents[0].Name != "architect" || r.Agents[0].System == "" || r.Agents[0].Ref != "" {
		t.Errorf("plain ref did not resolve: %+v", r.Agents[0])
	}
	// The specialized ref overrides tools/model, keeps identity.
	if len(r.Agents[1].Tools) != 2 || r.Agents[1].Model == nil || r.Agents[1].Name != "architect" {
		t.Errorf("specialized ref = %+v", r.Agents[1])
	}
	if r.Agents[2].Name != "inline" {
		t.Errorf("inline agent disturbed: %+v", r.Agents[2])
	}

	// Refs with inline identity are validation errors.
	bad := strings.Replace(roomYAML, "- persona: architect\n",
		"- persona: architect\n    name: not-architect\n", 1)
	if _, err := ParseRoom([]byte(bad)); err == nil || !strings.Contains(err.Error(), "references persona") {
		t.Errorf("conflicting ref = %v", err)
	}
	// Unknown references fail at resolution, with the library in the message.
	missing := strings.Replace(roomYAML, "persona: architect", "persona: ghost", 1)
	r2, err := ParseRoom([]byte(missing))
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.ResolvePersonas(lib); err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Errorf("unknown ref = %v", err)
	}
}

// LoadRoom resolves automatically: the personas/ directory next to the
// room file (and the workspace root's, then the global one) fills refs.
func TestLoadRoomResolvesFromLibrary(t *testing.T) {
	dir := t.TempDir()
	writePersona(t, filepath.Join(dir, "personas"), "reviewer.yaml", reviewerYAML)
	room := filepath.Join(dir, "rooms", "demo.yaml")
	writePersona(t, filepath.Join(dir, "rooms"), "demo.yaml",
		"name: demo\nagents:\n  - persona: reviewer\n")

	r, err := LoadRoom(room)
	if err != nil {
		t.Fatal(err)
	}
	if r.Agents[0].Name != "reviewer" || r.Agents[0].System == "" {
		t.Fatalf("agent did not resolve: %+v", r.Agents[0])
	}

	// Missing persona: a load error that says which persona and where
	// it looked.
	writePersona(t, filepath.Join(dir, "rooms"), "sad.yaml",
		"name: sad\nagents:\n  - persona: ghost\n")
	_, err = LoadRoom(filepath.Join(dir, "rooms", "sad.yaml"))
	if err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Fatalf("missing persona = %v", err)
	}
}
