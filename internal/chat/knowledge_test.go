package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// knowledgeMock scripts the knowledge maintainer's calls: a plan, one
// note, one digest — dispatched by request shape like the knowledge
// package's own tests.
func knowledgeMock(plan string) llm.Provider {
	return llm.NewMockFunc(func(req llm.Request) *llm.Response {
		if req.ResponseSchema != nil {
			return &llm.Response{Text: plan}
		}
		if req.MaxTokens == 700 {
			return &llm.Response{Text: "the note body"}
		}
		return &llm.Response{Text: "# digest\nbuilt"}
	})
}

const areaPlan = `{"areas":[{"name":"Core","scope":"the core","dirs":["pkg"],"files":[]}]}`

func TestKnowledgeMaintainsAfterWritingTurn(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# k\n\nproject\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// One tool-using agent: writes a file, then answers. An empty room
	// roster keeps observers (and their decision calls) out of the way.
	writer := &agent.Agent{
		Persona: config.Persona{Name: "builder", Role: "Builder", System: "build", Tools: []string{"write_file"}},
		Provider: llm.NewMock(
			&llm.Response{ToolCalls: []llm.ToolCall{{ID: "t1", Name: "write_file", Args: `{"path":"pkg/new.go","content":"package pkg"}`}}, StopReason: llm.StopToolUse},
			&llm.Response{Text: "done", StopReason: llm.StopEndTurn},
		),
		CWD: ws,
	}
	p := testPersona("builder", "m")
	p.Tools = []string{"write_file"}
	tr, err := OpenTranscript(filepath.Join(ws, ".loop", "rooms", "k"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Room{Name: "k", Agents: []config.Persona{p}}
	r := NewRoom(cfg, []*agent.Agent{writer}, tr)
	r.Workspace = ws
	r.SetKnowledge(&knowledge.Manager{WS: ws, Provider: knowledgeMock(areaPlan), Model: "mock", Meter: usage.NewMeter()})

	ui := &recorderUI{}
	if err := r.Say(context.Background(), "@builder make it so", ui); err != nil {
		t.Fatal(err)
	}

	// The refresh ran and was announced once.
	announcements := 0
	for _, m := range tr.Messages {
		if m.From == "system" && strings.Contains(m.Text, "◈ knowledge:") {
			announcements++
		}
	}
	if announcements != 1 {
		t.Fatalf("knowledge announcements = %d, want 1 (messages: %+v)", announcements, tr.Messages)
	}
	if _, ok := knowledge.ReadDigest(ws); !ok {
		t.Fatal("digest not built by post-turn maintenance")
	}

	// A turn that writes nothing spends nothing.
	before := r.Knowledge // same manager; assert via a fresh scan instead
	rep := knowledge.Scan(ws, knowledgeMustLoad(t, ws))
	if !rep.Fresh {
		t.Fatalf("layer not fresh after maintenance: %+v", rep)
	}
	_ = before
	if err := r.Say(context.Background(), "@builder just talk", ui); err != nil {
		t.Fatal(err)
	}
	for _, m := range tr.Messages {
		if m.From == "system" && strings.HasPrefix(m.Text, "◈ knowledge: knowledge up to date") {
			t.Fatal("no-op maintenance must stay silent")
		}
	}
}

func knowledgeMustLoad(t *testing.T, ws string) *knowledge.Index {
	t.Helper()
	idx, err := knowledge.Load(ws)
	if err != nil || idx == nil {
		t.Fatalf("load: %v", err)
	}
	return idx
}

func TestNotesVerb(t *testing.T) {
	ws := t.TempDir()
	p := testPersona("solo", "m")
	tr, _ := OpenTranscript(filepath.Join(ws, ".loop", "rooms", "n"))
	cfg := config.Room{Name: "n", Agents: []config.Persona{p}}
	r := NewRoom(cfg, []*agent.Agent{{Persona: p, Provider: llm.NewMock(&llm.Response{Text: "ok"})}}, tr)
	r.Workspace = ws
	r.SetKnowledge(&knowledge.Manager{WS: ws, Provider: knowledgeMock(areaPlan), Model: "mock"})

	ui := &recorderUI{}
	// Empty layer: a helpful line, no crash.
	if err := r.Say(context.Background(), "/notes", ui); err != nil {
		t.Fatal(err)
	}
	last := tr.Messages[len(tr.Messages)-1]
	if !strings.Contains(last.Text, "no project knowledge yet") {
		t.Fatalf("empty /notes = %q", last.Text)
	}

	// Seeded layer: index listing, then one note.
	idx := `{"version":1,"notes":[{"slug":"core","title":"Core","scope":"the core","dirs":["pkg"]}]}`
	kd := filepath.Join(ws, ".loop", "knowledge")
	os.MkdirAll(filepath.Join(kd, "notes"), 0o755)
	os.WriteFile(filepath.Join(kd, "index.json"), []byte(idx), 0o644)
	os.WriteFile(filepath.Join(kd, "notes", "core.md"), []byte("<!-- h -->\ncore note body"), 0o644)

	if err := r.Say(context.Background(), "/notes", ui); err != nil {
		t.Fatal(err)
	}
	last = tr.Messages[len(tr.Messages)-1]
	if !strings.Contains(last.Text, "core — Core: the core") {
		t.Fatalf("listing = %q", last.Text)
	}
	if err := r.Say(context.Background(), "/notes core", ui); err != nil {
		t.Fatal(err)
	}
	last = tr.Messages[len(tr.Messages)-1]
	if !strings.Contains(last.Text, "core note body") {
		t.Fatalf("note read = %q", last.Text)
	}
}

func TestKnowledgeKillSwitch(t *testing.T) {
	off := false
	cfg := config.Room{Name: "k", Agents: []config.Persona{testPersona("solo", "m")}}
	cfg.Settings.Knowledge = &off
	tr, _ := OpenTranscript(t.TempDir())
	r := NewRoom(cfg, nil, tr)
	r.SetKnowledge(&knowledge.Manager{WS: t.TempDir(), Provider: knowledgeMock(areaPlan), Model: "mock"})
	if r.Knowledge != nil {
		t.Fatal("settings.knowledge: false must decline the knowledge layer")
	}
	on := true
	cfg.Settings.Knowledge = &on
	tr2, _ := OpenTranscript(t.TempDir())
	r2 := NewRoom(cfg, nil, tr2)
	r2.SetKnowledge(&knowledge.Manager{WS: t.TempDir(), Provider: knowledgeMock(areaPlan), Model: "mock"})
	if r2.Knowledge == nil {
		t.Fatal("settings.knowledge: true must keep the layer")
	}
}
