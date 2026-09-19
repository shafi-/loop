package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// A room's spend must outlive its sessions: usage.json carries every
// past session's ledger into the next one, and /cost shows both depths
// plus the pipelines the room commanded.
func TestCostLifetimeAndPipelines(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".loop", "rooms", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A past session's ledger, as FlushUsage would have written it.
	base := usage.Total{Calls: 85, InputTokens: 60000, OutputTokens: 9000}
	if err := os.WriteFile(usageFile(dir), mustJSON(base), 0o644); err != nil {
		t.Fatal(err)
	}
	// A run this room commanded (any session): its spend is in its own
	// run record, mentioned from an ARCHIVED transcript.
	runID := "20260101-101010-abcd"
	runDir := filepath.Join(ws, ".loop", "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"done": true,
		"usage": map[string]any{
			"calls": 10, "input_tokens": 12000, "output_tokens": 2000,
		},
	}
	if err := os.WriteFile(filepath.Join(runDir, "state.json"), mustJSON(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript-20260101-101010.jsonl"),
		[]byte(`{"from":"deliver","text":"completed — run `+runID+`; records in .loop/runs/`+runID+`/"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := config.Persona{Name: "eng", Role: "Eng", System: "s"}
	mock := llm.NewMock(&llm.Response{Text: "ok"})
	r := NewRoom(config.Room{Name: "demo", Agents: []config.Persona{p}}, []*agent.Agent{{Persona: p, Provider: mock}}, tr)
	r.RunUsage = engine.ReadRunUsage
	r.Workspace = ws
	meter := usage.NewMeter()
	meter.Record("eng", llm.Usage{InputTokens: 412, OutputTokens: 38})
	r.Usage = meter

	report := r.CostReport()
	if !strings.Contains(report, "session usage — 1 calls · in 412 · out 38 tokens") {
		t.Errorf("session line missing:\n%s", report)
	}
	if !strings.Contains(report, "all sessions — 86 calls") {
		t.Errorf("all-sessions line must add the persisted base (85+1):\n%s", report)
	}
	if !strings.Contains(report, "pipelines — 10 calls") || !strings.Contains(report, "1 run commanded") {
		t.Errorf("pipelines line must sum the run record:\n%s", report)
	}

	// A turn flushes the lifetime ledger; the next session sees it.
	if err := r.Say(context.Background(), "@eng hi", noopUI{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(usageFile(dir))
	if err != nil {
		t.Fatal("Say must flush usage.json")
	}
	var got usage.Total
	if json.Unmarshal(data, &got) != nil || got.Calls != 86 || got.InputTokens != 60412 {
		t.Errorf("flushed ledger = %+v, want base+session (86 calls, 60,412 in)", got)
	}

	// The next session's /cost knows its history even before any call.
	tr2, _ := OpenTranscript(dir)
	r2 := NewRoom(config.Room{Name: "demo", Agents: []config.Persona{p}}, nil, tr2)
	r2.Workspace = ws
	r2.RunUsage = engine.ReadRunUsage
	r2.Usage = usage.NewMeter()
	if rep := r2.CostReport(); !strings.Contains(rep, "all sessions — 86 calls") {
		t.Errorf("fresh session should still know the lifetime:\n%s", rep)
	}
}

// Corrupt ledger files are a fresh start, never a broken room.
func TestCorruptUsageFileIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(usageFile(dir), []byte("{not json"), 0o644)
	if got := loadUsageBase(dir); got.Calls != 0 {
		t.Errorf("corrupt base = %+v, want zero", got)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
