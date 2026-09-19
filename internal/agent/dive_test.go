package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// The speak decision carries the dive dimensions so observers get dive
// detection inside the call they already make.
func TestDecideSpeakAsksDiveFields(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: `{"speak":true,"priority":4,"reason":"r","deep_dive":false,"dive_topic":""}`})
	a := &Agent{Persona: config.Persona{Name: "cfo", Role: "CFO"}, Provider: m}
	if _, err := a.DecideSpeak(context.Background(), []config.Persona{a.Persona}, "hello"); err != nil {
		t.Fatal(err)
	}
	req := m.Requests()[0]
	if req.ResponseSchema == nil {
		t.Fatal("structured call lost its schema")
	}
	props, _ := req.ResponseSchema["properties"].(map[string]any)
	for _, field := range []string{"deep_dive", "dive_topic"} {
		if _, ok := props[field]; !ok {
			t.Errorf("speak schema missing %q", field)
		}
	}
	required, _ := req.ResponseSchema["required"].([]string)
	for _, field := range []string{"deep_dive", "dive_topic"} {
		found := false
		for _, f := range required {
			if f == field {
				found = true
			}
		}
		if !found {
			t.Errorf("speak schema required list missing %q", field)
		}
	}
}

func TestDecideDive(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: `{"deep_dive":true,"dive_topic":"session invalidation in auth/","reason":"notes are thin"}`})
	a := &Agent{Persona: config.Persona{Name: "architect", Role: "Architect"}, Provider: m}
	d, err := a.DecideDive(context.Background(), "[ceo] why do sessions leak?")
	if err != nil {
		t.Fatal(err)
	}
	if !d.DeepDive || d.Topic != "session invalidation in auth/" {
		t.Errorf("decision = %+v", d)
	}
	req := m.Requests()[0]
	if req.ResponseSchema["properties"] == nil {
		t.Error("dive call lost its schema")
	}
	if !strings.Contains(divePromptFor(t, req), "why do sessions leak?") {
		t.Error("dive prompt lost the conversation")
	}
}

// The dive prompt quotes the conversation, so the decision is about the
// actual question.
func divePromptFor(t *testing.T, req llm.Request) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("no messages")
	}
	return req.Messages[0].Content
}

func TestDecideDiveBadJSON(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: `not json`})
	a := &Agent{Persona: config.Persona{Name: "architect"}, Provider: m}
	if _, err := a.DecideDive(context.Background(), "hello"); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("err = %v", err)
	}
}

// Base exposes the composed identity so a dive run carries the same
// grounding as a reply: persona system, workspace, knowledge, framing.
func TestBaseComposesGrounding(t *testing.T) {
	a := &Agent{
		Persona:   config.Persona{Name: "architect", Role: "Architect", System: "design it"},
		Workspace: "the brief",
		Knowledge: func() string { return "the digest" },
	}
	room := []config.Persona{a.Persona, {Name: "reviewer", Role: "Reviewer"}}
	base := a.Base(room)
	for _, want := range []string{"design it", "the brief", "the digest", "architect, the Architect", "reviewer"} {
		if !strings.Contains(base, want) {
			t.Errorf("base missing %q:\n%s", want, base)
		}
	}
}
