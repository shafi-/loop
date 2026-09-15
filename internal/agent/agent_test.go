package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// Room agents resolve their model through the same env-first rule as
// everything else: a persona with no model block follows the configured
// family; an explicit model id wins. Pinned here because Reply and
// DecideSpeak both put this value on the wire.
func TestAgentModelResolution(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_MODEL", "env-chosen-model")

	newAgent := func(m *config.ModelConfig) *Agent {
		return &Agent{
			Persona:  config.Persona{Name: "ceo", Role: "CEO", Model: m},
			Provider: llm.NewMock(&llm.Response{Text: "ok"}),
		}
	}

	// No model block: env-driven (openai family inferred from OPENAI_* env).
	a := newAgent(nil)
	resp, err := a.Reply(context.Background(), nil, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp != "ok" {
		t.Fatalf("reply = %q", resp)
	}
	got := a.Provider.(*llm.Mock).Requests()[0].Model
	if got != "env-chosen-model" {
		t.Errorf("model-less persona must send OPENAI_MODEL, got %q", got)
	}

	// Explicit model id beats the env.
	a = newAgent(&config.ModelConfig{Provider: config.ProviderOpenAI, Model: "yaml-model"})
	if _, err := a.Reply(context.Background(), nil, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if got := a.Provider.(*llm.Mock).Requests()[0].Model; got != "yaml-model" {
		t.Errorf("explicit persona model must beat env, got %q", got)
	}

	// Anthropic personas read the anthropic env, not the openai one.
	a = newAgent(&config.ModelConfig{Provider: config.ProviderAnthropic})
	if _, err := a.Reply(context.Background(), nil, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if got := a.Provider.(*llm.Mock).Requests()[0].Model; got != llm.DefaultModel["anthropic"] {
		t.Errorf("anthropic persona without ANTHROPIC_MODEL must use the built-in default, got %q", got)
	}

	// The speak decision rides the same resolution (fresh mock: the
	// decision must be valid JSON, not the reply text).
	dMock := llm.NewMock(&llm.Response{Text: `{"speak": true, "priority": 3, "reason": "cost angle"}`})
	a = &Agent{
		Persona:  config.Persona{Name: "cfo", Role: "CFO", Model: &config.ModelConfig{Provider: config.ProviderAnthropic}},
		Provider: dMock,
	}
	if _, err := a.DecideSpeak(context.Background(), nil, "", "new message"); err != nil {
		t.Fatal(err)
	}
	last := dMock.Requests()[0]
	if last.Model != llm.DefaultModel["anthropic"] {
		t.Errorf("DecideSpeak must use the same resolved model, got %q", last.Model)
	}
	if last.ResponseSchema == nil {
		t.Error("DecideSpeak must keep its structured-output schema")
	}
	if !strings.Contains(last.Messages[0].Content, "new message") {
		t.Error("DecideSpeak must include the new message in its prompt")
	}
}
