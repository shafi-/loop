package config

import (
	"strings"
	"testing"
)

// The two new contradictions: a model on a non-gate human stage, and a
// terminal router. Validation only rejects contradictions — the new
// fields are optional everywhere else.
func TestRoomAgentToolsValidated(t *testing.T) {
	ok := `
name: r
agents:
  - name: architect
    role: Architect
    system: terse
    tools: [read_file, write_file]
`
	if _, err := ParseRoom([]byte(ok)); err != nil {
		t.Fatalf("valid room tools rejected: %v", err)
	}
	bad := strings.Replace(ok, "[read_file, write_file]", "[read_file, skynet]", 1)
	if _, err := ParseRoom([]byte(bad)); err == nil {
		t.Error("unknown room tool must be rejected")
	}
}

func TestGateAndTerminalValidation(t *testing.T) {
	src := `
name: p
stages:
  - id: approval
    type: human
    gate: true
    prompt: ok?
  - id: ship
    type: tool
    terminal: true
    run: echo ship
`
	if _, err := ParsePipeline([]byte(src)); err != nil {
		t.Fatalf("valid gate + terminal pipeline rejected: %v", err)
	}

	if _, err := ParsePipeline([]byte(`
name: p
stages:
  - id: approval
    type: human
    prompt: ok?
    model: {provider: anthropic}
`)); err == nil {
		t.Error("model without gate: true must be rejected")
	}

	if _, err := ParsePipeline([]byte(`
name: p
stages:
  - id: gate
    type: router
    terminal: true
    when:
      - next: gate
`)); err == nil {
		t.Error("terminal router must be rejected")
	}
}
