package examples

import (
	"os"
	"path/filepath"
	"strings"
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
		// Personas ship their seat's tools: the code seats can act in
		// any room that references them, the business seats never do —
		// the CFO argues from judgment, it does not read code.
		want := map[string][][]string{
			"architect":     {{"read_file", "write_file", "run_command"}},
			"engineer":      {{"read_file", "write_file", "run_command"}},
			"reviewer":      {{"read_file"}},
			"cfo":           {nil},
			"product-owner": {nil},
			"end-user":      {nil},
		}
		allowed := want[name]
		ok := false
		for _, w := range allowed {
			if strings.Join(w, ",") == strings.Join(p.Tools, ",") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("persona %s ships with tools %v — want one of %v", name, p.Tools, allowed)
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
	// Tools come from the library now, not the room: the engineer and
	// architect resolve with the full working set, the reviewer stays
	// read-only — a room that wants different powers overrides with
	// tools: on the agent entry.
	var engineer, architect, reviewer *config.Persona
	for i := range room.Agents {
		switch room.Agents[i].Name {
		case "engineer":
			engineer = &room.Agents[i]
		case "architect":
			architect = &room.Agents[i]
		case "reviewer":
			reviewer = &room.Agents[i]
		}
	}
	if engineer == nil || architect == nil || reviewer == nil {
		t.Fatal("a shipped seat missing after resolution")
	}
	if engineer.System == "" {
		t.Fatal("engineer resolved without a system prompt")
	}
	if len(engineer.Tools) != 3 {
		t.Fatalf("engineer tools = %v, want the library's read/write/run set", engineer.Tools)
	}
	if len(architect.Tools) != 3 {
		t.Fatalf("architect tools = %v, want the library's read/write/run set", architect.Tools)
	}
	if len(reviewer.Tools) != 1 || reviewer.Tools[0] != "read_file" {
		t.Fatalf("reviewer tools = %v, want read-only", reviewer.Tools)
	}
}

func TestStarterFeatureRoomResolves(t *testing.T) {
	if _, err := config.ParseRoom([]byte(FeatureRoom)); err != nil {
		t.Fatalf("feature room no longer validates: %v", err)
	}
	lib, _ := seedLibrary(t)
	room, err := config.ParseRoom([]byte(FeatureRoom))
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ResolvePersonas(lib); err != nil {
		t.Fatalf("feature room persona references do not resolve: %v", err)
	}
	// The shaping council, not the build team: the business seats carry
	// no tools — shaping is talk, and the CFO never reads code. The
	// architect keeps its library defaults (read/write/run) so
	// feasibility can be verified or deep-dived on request.
	want := map[string]bool{"product-owner": true, "cfo": true, "end-user": true, "architect": true}
	got := map[string]bool{}
	for i := range room.Agents {
		got[room.Agents[i].Name] = true
		name := room.Agents[i].Name
		if name == "architect" {
			if len(room.Agents[i].Tools) != 3 {
				t.Errorf("feature room architect tools = %v, want the library defaults", room.Agents[i].Tools)
			}
			continue
		}
		if len(room.Agents[i].Tools) > 0 {
			t.Errorf("feature room grants %v to %s — business seats carry no tools", room.Agents[i].Tools, name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("feature room lost its %q seat", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("feature room seats = %v, want exactly %v", got, want)
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

// The implement pipeline consumes the room's approved plan: a tool
// stage discovers the newest .loop/plans/*.md, the engineer agent treats it
// as the source of truth, and the gated revise loop listens.
func TestStarterImplementPipelineShape(t *testing.T) {
	p, err := config.ParsePipeline([]byte(ImplementPipeline))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]config.StageType{}
	for i := range p.Stages {
		ids[p.Stages[i].ID] = p.Stages[i].Type
	}
	for _, id := range []string{"plan-file", "room-context", "implement", "review", "approval", "revise"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("implement pipeline lost its %q stage", id)
		}
	}
	if ids["plan-file"] != config.StageTool {
		t.Errorf("plan-file stage is %v, want a deterministic tool stage", ids["plan-file"])
	}
	if ids["room-context"] != config.StageTool {
		t.Errorf("room-context stage is %v, want a deterministic tool stage", ids["room-context"])
	}
	// Every agent stage references the shipped engineer, and the
	// implement input carries the discovered plan file.
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type == config.StageAgent && s.Agent.Persona != "engineer" {
			t.Errorf("agent stage %q uses persona %q, want the shipped engineer", s.ID, s.Agent.Persona)
		}
	}
	var input string
	for i := range p.Stages {
		if p.Stages[i].ID == "implement" {
			input = p.Stages[i].Agent.Input
		}
	}
	if !strings.Contains(input, "${stages.plan-file.output}") {
		t.Errorf("implement input does not consume the discovered plan file:\n%s", input)
	}
	if !strings.Contains(input, "${stages.room-context.output}") {
		t.Errorf("implement input does not consume the room's hand-off:\n%s", input)
	}
}
