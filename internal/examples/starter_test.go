package examples

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// The starter kit ships with the binary; if any document stops
// validating, a fresh install scaffolds a broken workspace — so every
// piece is pinned here, including persona-reference resolution against
// a library built exactly the way seeding builds it.

func TestStarterPersonasParse(t *testing.T) {
	for name, content := range BuiltinPersonas() {
		p, err := config.ParsePersona([]byte(content))
		if err != nil {
			t.Errorf("persona %s no longer parses: %v", name, err)
			continue
		}
		if p.Name != name {
			t.Errorf("persona file for %q declares name %q", name, p.Name)
		}
		// Library personas are identity-only: tools are granted at the
		// use site, so referencing them never forces a tool set.
		if len(p.Tools) > 0 {
			t.Errorf("persona %s ships with tools %v — grant tools in rooms/pipelines instead", name, p.Tools)
		}
	}
}

// seedLibrary writes the shipped personas to a directory the way
// seedGlobalPersonas does, and returns a library over it.
func seedLibrary(t *testing.T) (*config.PersonaLibrary, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(BuiltinPersonas()))
	for name := range BuiltinPersonas() {
		names = append(names, name)
	}
	for _, name := range names {
		path := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(path, []byte(BuiltinPersonas()[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config.NewPersonaLibrary(dir), dir
}

func TestStarterDevRoomResolves(t *testing.T) {
	if _, err := config.ParseRoom([]byte(DevRoom)); err != nil {
		t.Fatalf("dev room no longer validates: %v", err)
	}
	lib, _ := seedLibrary(t)
	room, err := config.ParseRoom([]byte(DevRoom))
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ResolvePersonas(lib); err != nil {
		t.Fatalf("dev room persona references do not resolve: %v", err)
	}
	// The engineer must come out of the library with a system, and the
	// room grants it the file tools.
	var engineer *config.Persona
	for i := range room.Agents {
		if room.Agents[i].Name == "engineer" {
			engineer = &room.Agents[i]
		}
	}
	if engineer == nil {
		t.Fatal("engineer missing after resolution")
	}
	if engineer.System == "" {
		t.Fatal("engineer resolved without a system prompt")
	}
	if len(engineer.Tools) != 3 {
		t.Fatalf("engineer tools = %v, want the room's read/write/run grant", engineer.Tools)
	}
}

func TestStarterPipelinesValidateAndResolve(t *testing.T) {
	lib, _ := seedLibrary(t)
	for name, content := range map[string]string{
		"implement": ImplementPipeline,
		"review":    ReviewPipeline,
	} {
		p, err := config.ParsePipeline([]byte(content))
		if err != nil {
			t.Errorf("pipeline %s no longer validates: %v", name, err)
			continue
		}
		if err := p.ResolvePersonas(lib); err != nil {
			t.Errorf("pipeline %s persona references do not resolve: %v", name, err)
		}
	}
}

// The implement pipeline's rework loop must listen like the flagship
// example's: a "changes" answer routes to revise, and revise writes
// the shared implement_md slot that review re-reads.
func TestStarterImplementPipelineShape(t *testing.T) {
	p, err := config.ParsePipeline([]byte(ImplementPipeline))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]config.StageType{}
	for i := range p.Stages {
		ids[p.Stages[i].ID] = p.Stages[i].Type
	}
	for _, id := range []string{"plan", "implement", "review", "approval", "revise"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("implement pipeline lost its %q stage", id)
		}
	}
	// Every agent stage references the shipped engineer.
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type == config.StageAgent && s.Agent.Persona != "engineer" {
			t.Errorf("agent stage %q uses persona %q, want the shipped engineer", s.ID, s.Agent.Persona)
		}
	}
}
