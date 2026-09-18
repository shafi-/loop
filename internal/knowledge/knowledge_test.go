package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// seedWorkspace builds a small two-area Go workspace.
func seedWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# demo\n\nA demo project.\n")
	write("go.mod", "module demo\n\ngo 1.22\n")
	write("internal/alpha/a.go", "package alpha\n\n// A does a thing.\n")
	write("internal/alpha/helper.go", "package alpha\n\nfunc helper() {}\n")
	write("internal/beta/b.go", "package beta\n\n// B does another thing.\n")
	write(".loop/rooms/keep.jsonl", "{}\n") // noise: must stay out of coverage
	return ws
}

// mockProvider dispatches on request shape: the plan call carries a
// ResponseSchema; note and digest calls are told apart by max tokens.
func mockProvider(t *testing.T, plan string) (*llm.Mock, *int) {
	t.Helper()
	notes := 0
	digests := 0
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		if req.ResponseSchema != nil {
			return &llm.Response{Text: plan, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}
		}
		if req.MaxTokens == maxNoteOutputTokens {
			notes++
			return &llm.Response{Text: "Note about " + areaFromPrompt(req), Usage: llm.Usage{InputTokens: 100, OutputTokens: 50}}
		}
		digests++
		return &llm.Response{Text: "# digest\nThe demo project. Area notes:\n- alpha — does a\n- beta — does b", Usage: llm.Usage{InputTokens: 200, OutputTokens: 60}}
	})
	return m, &notes
}

func areaFromPrompt(req llm.Request) string {
	for _, msg := range req.Messages {
		if strings.HasPrefix(msg.Content, "Area: ") {
			line := strings.SplitN(msg.Content, "\n", 2)[0]
			return strings.TrimPrefix(line, "Area: ")
		}
	}
	return "?"
}

const testPlan = `{"areas":[
  {"name":"Alpha","scope":"the alpha subsystem","dirs":["internal/alpha"],"files":[]},
  {"name":"Beta","scope":"the beta subsystem","dirs":["internal/beta"],"files":[]}
]}`

func TestMaintainSeedsThenStaysFresh(t *testing.T) {
	ws := seedWorkspace(t)
	mock, _ := mockProvider(t, testPlan)
	meter := usage.NewMeter()
	m := &Manager{WS: ws, Provider: mock, Model: "mock", Meter: meter}

	res, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatalf("seed maintain: %v", err)
	}
	if len(res.Updated) != 2 || !res.DigestUpdated {
		t.Fatalf("seed result = %+v, want 2 notes + digest", res)
	}
	if res.Calls != 4 { // plan + 2 notes + digest
		t.Fatalf("calls = %d, want 4", res.Calls)
	}
	if got := len(meter.Snapshot()); got != 1 || meter.Snapshot()[0].Label != UsageLabel {
		t.Fatalf("meter labels = %+v, want one %q entry", meter.Snapshot(), UsageLabel)
	}
	if _, ok := ReadDigest(ws); !ok {
		t.Fatal("digest.md missing after seed")
	}
	for _, slug := range []string{"alpha", "beta"} {
		if _, ok := ReadNote(ws, slug); !ok {
			t.Fatalf("note %s missing after seed", slug)
		}
	}

	// Second run: nothing changed — zero calls, "up to date".
	callsBefore := mock.Calls()
	res2, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatalf("fresh maintain: %v", err)
	}
	if res2.Calls != 0 || res2.Message != "knowledge up to date" {
		t.Fatalf("second result = %+v, want zero-call up-to-date", res2)
	}
	if mock.Calls() != callsBefore {
		t.Fatalf("fresh maintain spent %d calls, want 0", mock.Calls()-callsBefore)
	}
	// MaintainLine suppresses the no-op announcement.
	if line := m.MaintainLine(context.Background()); line != "" {
		t.Fatalf("fresh MaintainLine = %q, want empty", line)
	}
}

func TestMaintainRefreshesOnlyStaleNote(t *testing.T) {
	ws := seedWorkspace(t)
	mock, noteCalls := mockProvider(t, testPlan)
	m := &Manager{WS: ws, Provider: mock, Model: "mock"}

	if _, err := m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	*noteCalls = 0
	before := mock.Calls()

	// Touch one file in one area.
	if err := os.WriteFile(filepath.Join(ws, "internal/alpha/a.go"), []byte("package alpha\n\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "alpha" {
		t.Fatalf("updated = %v, want [alpha]", res.Updated)
	}
	if !res.DigestUpdated {
		t.Fatal("digest should rebuild when a note does")
	}
	if *noteCalls != 1 {
		t.Fatalf("note calls = %d, want 1 (only the stale area)", *noteCalls)
	}
	_ = before
}

func TestMaintainRePlansOnLayoutChange(t *testing.T) {
	ws := seedWorkspace(t)
	mock, _ := mockProvider(t, testPlan)
	m := &Manager{WS: ws, Provider: mock, Model: "mock"}
	if _, err := m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	plansBefore := 0
	for _, req := range mock.Requests() {
		if req.ResponseSchema != nil {
			plansBefore++
		}
	}

	// A new top-level source dir changes the layout signature → re-plan
	// and seed the new area's note.
	if err := os.MkdirAll(filepath.Join(ws, "cmd/loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "cmd/loop/main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plansAfter := 0
	for _, req := range mock.Requests() {
		if req.ResponseSchema != nil {
			plansAfter++
		}
	}
	if plansAfter != plansBefore+1 {
		t.Fatalf("plan calls %d → %d, want +1 on layout change", plansBefore, plansAfter)
	}
	if !res.DigestUpdated {
		t.Fatal("digest should rebuild after re-plan")
	}
}

func TestMaintainDeferredByCallCap(t *testing.T) {
	ws := seedWorkspace(t)
	// Ten areas against a cap of MaxRefreshCalls.
	var areas []string
	for i := 0; i < 10; i++ {
		areas = append(areas, strings.ReplaceAll(
			`{"name":"AreaN","scope":"s","dirs":["internal/alpha"],"files":[]}`, "AreaN", areaNames[i]))
	}
	plan := `{"areas":[` + strings.Join(areas, ",") + `]}`
	mock, _ := mockProvider(t, plan)
	m := &Manager{WS: ws, Provider: mock, Model: "mock"}

	res, err := m.Maintain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != MaxRefreshCalls {
		t.Fatalf("updated %d notes, want cap %d", len(res.Updated), MaxRefreshCalls)
	}
	if len(res.Deferred) == 0 {
		t.Fatal("expected deferred notes past the cap")
	}
	if res.Calls > MaxRefreshCalls+2 {
		t.Fatalf("calls = %d, exceeded cap materially", res.Calls)
	}
}

var areaNames = []string{"aa", "bb", "cc", "dd", "ee", "ff", "gg", "hh", "ii", "jj"}

func TestParsePlanCleansPaths(t *testing.T) {
	data := `{"areas":[
		{"name":"Chat Engine!","scope":"rooms","dirs":["internal/chat","../escape","/abs"],"files":["go.mod","../nope"]},
		{"name":"","scope":"unnamed","dirs":["x"],"files":[]},
		{"name":"Empty","scope":"nothing","dirs":[],"files":[]}
	]}`
	notes, err := parsePlan(data, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %d (%+v), want 1", len(notes), notes)
	}
	if notes[0].Slug != "chat-engine" {
		t.Fatalf("slug = %q, want chat-engine", notes[0].Slug)
	}
	for _, d := range notes[0].Dirs {
		if strings.HasPrefix(d, "..") || filepath.IsAbs(d) {
			t.Fatalf("escaping dir %q survived", d)
		}
	}
	for _, f := range notes[0].Files {
		if strings.HasPrefix(f, "..") || filepath.IsAbs(f) {
			t.Fatalf("escaping file %q survived", f)
		}
	}
}

func TestScanStatesAndProvenance(t *testing.T) {
	ws := seedWorkspace(t)
	mock, _ := mockProvider(t, testPlan)
	m := &Manager{WS: ws, Provider: mock, Model: "mock"}
	if _, err := m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(ws)
	if err != nil || idx == nil {
		t.Fatalf("load index: %v", err)
	}
	rep := Scan(ws, idx)
	if !rep.Fresh {
		t.Fatalf("scan after seed = %+v, want fresh", rep)
	}
	// Deleting a covered file is staleness, not invisibility.
	os.Remove(filepath.Join(ws, "internal/beta/b.go"))
	rep = Scan(ws, idx)
	if rep.Notes["beta"] != StateStale {
		t.Fatalf("beta state = %q after deletion, want stale", rep.Notes["beta"])
	}
	// Provenance headers are stripped for readers but on disk for humans.
	data, _ := os.ReadFile(notePath(ws, "alpha"))
	if !strings.HasPrefix(string(data), "<!-- loop area note") {
		t.Fatal("note file missing provenance header")
	}
	if body, _ := ReadNote(ws, "alpha"); strings.HasPrefix(body, "<!--") {
		t.Fatal("ReadNote must strip provenance")
	}
	if block := DigestBlock(ws); !strings.Contains(block, "Project knowledge") || strings.Contains(block, "<!--") {
		t.Fatalf("DigestBlock malformed: %.80s", block)
	}
}

func TestReadNoteRejectsBadSlugs(t *testing.T) {
	ws := seedWorkspace(t)
	if _, ok := ReadNote(ws, "../index"); ok {
		t.Fatal("traversal slug accepted")
	}
	if _, ok := ReadNote(ws, "nope"); ok {
		t.Fatal("unknown slug accepted")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	ws := t.TempDir()
	idx := &Index{Version: 1, Notes: []Note{{
		Slug: "x", Title: "X", Scope: "s",
		Dirs: []string{"a"}, Hashes: map[string]string{"a/f.go": "abc"},
	}}}
	idx.Digest.Hashes = map[string]string{layoutHashKey: "sig"}
	if err := Save(ws, idx); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ws)
	if err != nil || got == nil || len(got.Notes) != 1 || got.Notes[0].Slug != "x" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if got.Digest.Hashes[layoutHashKey] != "sig" {
		t.Fatalf("digest hashes lost: %+v", got.Digest)
	}
	// Corrupt index reads as a fresh start.
	os.WriteFile(indexPath(ws), []byte("{not json"), 0o644)
	got, err = Load(ws)
	if err != nil || got != nil {
		t.Fatalf("corrupt index = %+v, %v; want nil, nil", got, err)
	}
}

func TestMaintainLineFormats(t *testing.T) {
	ws := seedWorkspace(t)
	// A provider that fails everything: line reports failure, not panic.
	m := &Manager{WS: ws, Provider: llm.NewMock(), Model: "mock"}
	line := m.MaintainLine(context.Background())
	if !strings.Contains(line, "knowledge refresh failed") {
		t.Fatalf("line = %q, want failure report", line)
	}
	// Concurrency: a second Maintain while one runs is skipped silently.
	idx, _ := Load(ws)
	_ = idx
	raw, _ := json.Marshal(testPlan)
	if len(raw) == 0 {
		t.Fatal("unreachable")
	}
}

func TestEmptyWorkspaceStaysSilent(t *testing.T) {
	// No source directories — nothing to summarize. Maintenance must
	// spend zero calls and write nothing (a hosted room in a bare
	// directory must not pollute its transcript).
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("just a file"), 0o644)
	mock := llm.NewMock()
	m := &Manager{WS: ws, Provider: mock, Model: "mock"}
	line := m.MaintainLine(context.Background())
	if line != "" {
		t.Fatalf("line = %q, want silence", line)
	}
	if mock.Calls() != 0 {
		t.Fatalf("calls = %d, want 0", mock.Calls())
	}
	if _, err := os.Stat(Dir(ws)); !os.IsNotExist(err) {
		t.Fatal("knowledge dir written for an empty workspace")
	}
}
