package generator

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/llm"
)

// A minimal but genuinely valid pipeline in JSON form, as we instruct the
// model to emit.
const goodPipelineJSON = `{
  "name": "watch-csvs",
  "vars": {"dir": "./data"},
  "stages": [
    {"id": "scan", "type": "tool", "run": "ls {{ vars.dir }}", "output": "files"},
    {"id": "summarize", "type": "llm",
     "model": {"provider": "anthropic", "model": "claude-sonnet-4-5"},
     "prompt": "Summarize: ${stages.scan.output}"},
    {"id": "confirm", "type": "human", "prompt": "Proceed with: ${stages.summarize.output}"},
    {"id": "gate", "type": "router", "when": [
      {"if": "${stages.confirm.answer} == 'yes'", "next": "apply"},
      {"next": "summarize"}
    ]},
    {"id": "apply", "type": "tool", "run": "echo applied"}
  ]
}`

func draft(text string) *llm.Response {
	return &llm.Response{Text: text, StopReason: llm.StopEndTurn}
}

func gen(provider llm.Provider, repairs int) *Generator {
	return &Generator{Provider: provider, Model: "test-model", MaxRepairs: repairs}
}

func TestGenerateFirstDraftValid(t *testing.T) {
	m := llm.NewMock(draft(goodPipelineJSON))
	res, err := gen(m, 2).Generate(context.Background(), "watch a folder of CSVs")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pipeline.Name != "watch-csvs" || len(res.Pipeline.Stages) != 5 {
		t.Errorf("pipeline = %+v", res.Pipeline)
	}
	if !strings.Contains(string(res.YAML), "name: watch-csvs") {
		t.Errorf("YAML rendering = %q", res.YAML)
	}
	// The prompt must contain the contract and the user's description.
	req := m.Requests()[0]
	if !strings.Contains(req.Messages[0].Content, "watch a folder of CSVs") ||
		!strings.Contains(req.Messages[0].Content, "loop pipeline contract") {
		t.Error("draft prompt should carry the contract and the description")
	}
}

func TestGenerateToleratesFencesAndProse(t *testing.T) {
	wrapped := "Sure! Here's your pipeline:\n\n```json\n" + goodPipelineJSON + "\n```\n\nHope this helps!"
	m := llm.NewMock(draft(wrapped))
	res, err := gen(m, 0).Generate(context.Background(), "watch CSVs")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pipeline.Name != "watch-csvs" {
		t.Errorf("extraction failed: %+v", res.Pipeline)
	}
}

func TestGenerateRepairsFromValidationErrors(t *testing.T) {
	// First draft: missing prompt (invalid). Second draft: fixed.
	attempt := 0
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		attempt++
		if attempt == 1 {
			return draft(`{"name": "broken", "stages": [{"id": "a", "type": "llm",
				"model": {"provider": "anthropic", "model": "claude-sonnet-4-5"}}]}`)
		}
		// The repair prompt must quote the validation problem.
		if !strings.Contains(req.Messages[0].Content, "stages[0].prompt") {
			panic("repair prompt should contain the validator's error")
		}
		return draft(goodPipelineJSON)
	})
	res, err := gen(m, 2).Generate(context.Background(), "watch CSVs")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pipeline.Name != "watch-csvs" || attempt != 2 {
		t.Errorf("repair loop failed: attempt=%d pipeline=%+v", attempt, res.Pipeline)
	}
}

func TestGenerateGivesUpHonesty(t *testing.T) {
	m := llm.NewMock(draft(`{"name": "broken", "stages": []}`))
	_, err := gen(m, 2).Generate(context.Background(), "watch CSVs")
	if err == nil || !strings.Contains(err.Error(), "2 repair pass") {
		t.Fatalf("expected honest failure after repair budget, got: %v", err)
	}
	if m.Calls() != 3 { // 1 draft + 2 repairs
		t.Errorf("expected 3 calls, got %d", m.Calls())
	}
}

func TestGenerateRepairsUnparseableOutput(t *testing.T) {
	attempt := 0
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		attempt++
		if attempt == 1 {
			return draft("I cannot help with that.")
		}
		return draft(goodPipelineJSON)
	})
	res, err := gen(m, 1).Generate(context.Background(), "watch CSVs")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pipeline.Name != "watch-csvs" {
		t.Errorf("expected recovery, got %+v", res.Pipeline)
	}
}

// A minimal but genuinely valid room in JSON form, as we instruct the
// model to emit.
const goodRoomJSON = `{
  "name": "incident",
  "agents": [
    {"name": "commander", "role": "incident commander",
     "system": "You are the incident commander. You drive the response."},
    {"name": "sre", "role": "site reliability engineer",
     "system": "You are the SRE on call. You answer with facts and numbers.",
     "tools": ["read_file"]}
  ],
  "settings": {"speak_threshold": 0.6, "max_spontaneous_replies": 2, "history_window": 50}
}`

func TestGenerateRoomFirstDraftValid(t *testing.T) {
	m := llm.NewMock(draft(goodRoomJSON))
	res, err := gen(m, 2).GenerateRoom(context.Background(), "an incident response team", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Room == nil || res.Pipeline != nil {
		t.Fatalf("room draft should fill Room, not Pipeline: %+v", res)
	}
	if res.Room.Name != "incident" || len(res.Room.Agents) != 2 {
		t.Errorf("room = %+v", res.Room)
	}
	if !strings.Contains(string(res.YAML), "name: incident") {
		t.Errorf("YAML rendering = %q", res.YAML)
	}
	// The prompt must carry the room contract, the example, and the
	// description — and must forbid model blocks.
	prompt := m.Requests()[0].Messages[0].Content
	if !strings.Contains(prompt, "an incident response team") ||
		!strings.Contains(prompt, "loop room contract") ||
		!strings.Contains(prompt, "NEVER emit `model`") {
		t.Error("room draft prompt should carry contract, example, and description")
	}
}

func TestGenerateRoomRepairsFromValidationErrors(t *testing.T) {
	// First draft: duplicate agent name (invalid). Second: fixed.
	attempt := 0
	m := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		attempt++
		if attempt == 1 {
			return draft(`{"name": "broken", "agents": [
				{"name": "a", "system": "You are A."},
				{"name": "a", "system": "You are A again."}]}`)
		}
		if !strings.Contains(req.Messages[0].Content, "agents[1].name") {
			panic("room repair prompt should contain the validator's error")
		}
		return draft(goodRoomJSON)
	})
	res, err := gen(m, 2).GenerateRoom(context.Background(), "a small team", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Room == nil || res.Room.Name != "incident" || attempt != 2 {
		t.Errorf("room repair loop failed: attempt=%d room=%+v", attempt, res.Room)
	}
}

func TestGenerateRoomGivesUpHonesty(t *testing.T) {
	m := llm.NewMock(draft(`{"name": "broken", "agents": []}`))
	_, err := gen(m, 2).GenerateRoom(context.Background(), "a small team", nil)
	if err == nil || !strings.Contains(err.Error(), "valid room") {
		t.Fatalf("expected honest room failure, got: %v", err)
	}
}

// The room prompt teaches the draft about the workspace's persona
// library so it references known personas instead of reinventing them.
func TestGenerateRoomPromptListsAvailablePersonas(t *testing.T) {
	m := llm.NewMock(draft(goodRoomJSON))
	_, err := gen(m, 0).GenerateRoom(context.Background(), "a team", []string{"architect", "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := m.Requests()[0].Messages[0].Content
	if !strings.Contains(prompt, "AVAILABLE PERSONAS") ||
		!strings.Contains(prompt, "architect, reviewer") {
		t.Errorf("prompt should list the library personas: %.400s", prompt)
	}
}
