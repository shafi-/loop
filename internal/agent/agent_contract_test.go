package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// Confidence is the speak-threshold math: priority 1..5 maps to 0..1,
// out-of-range values clamp. The documented example (threshold 0.6 →
// priority 4+ qualifies) must hold.
func TestDecisionConfidence(t *testing.T) {
	cases := []struct {
		priority int
		want     float64
	}{
		{0, 0}, // below range clamps to silence
		{1, 0},
		{2, 0.25},
		{3, 0.5},
		{4, 0.75},
		{5, 1},
		{9, 1}, // above range clamps to certain
	}
	for _, c := range cases {
		d := Decision{Priority: c.priority}
		if got := d.Confidence(); got != c.want {
			t.Errorf("Confidence(priority %d) = %v, want %v", c.priority, got, c.want)
		}
	}
	// The manual's worked example: threshold 0.6 admits priority 4+.
	if (Decision{Priority: 4}.Confidence() < 0.6) || (Decision{Priority: 3}.Confidence() >= 0.6) {
		t.Error("threshold 0.6 must admit priority 4+ and nothing lower")
	}
}

func TestNameAndFramingFallbacks(t *testing.T) {
	room := []config.Persona{
		{Name: "ceo", Role: "CEO"},
		{Name: "cfo", Role: "CFO"},
		{Name: "architect"}, // no role — framing falls back to "colleague"
	}
	a := &Agent{Persona: room[2]}
	if a.Name() != "architect" {
		t.Errorf("Name() = %q", a.Name())
	}
	f := a.framing(room)
	if !strings.Contains(f, "the colleague,") {
		t.Errorf("empty role should fall back to colleague: %q", f)
	}
	if !strings.Contains(f, "with: ceo, cfo.") {
		t.Errorf("framing must list the others: %q", f)
	}
}

type errProvider struct{ err error }

func (p errProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, p.err
}
func (p errProvider) Stream(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, p.err
}

func TestReplyAndDecideSpeakErrorPaths(t *testing.T) {
	a := &Agent{
		Persona:  config.Persona{Name: "solo", Role: "Loner", System: "terse"},
		Provider: errProvider{err: errors.New("outage")},
	}
	if _, err := a.Reply(context.Background(), nil, "hi", nil); err == nil {
		t.Error("Reply must surface provider errors")
	}
	if _, err := a.DecideSpeak(context.Background(), nil, "before"); err == nil {
		t.Error("DecideSpeak must surface provider errors")
	}
}

func TestDecideSpeakClampsAndRejectsBadJSON(t *testing.T) {
	room := []config.Persona{{Name: "solo", Role: "Loner"}}

	clamp := llm.NewMock(&llm.Response{Text: `{"speak":true,"priority":9,"reason":"urgent"}`, StopReason: llm.StopEndTurn})
	a := &Agent{Persona: room[0], Provider: clamp}
	d, err := a.DecideSpeak(context.Background(), room, "[user] the roof is on fire")
	if err != nil {
		t.Fatal(err)
	}
	if d.Priority != 5 || !d.Speak {
		t.Errorf("priority 9 must clamp to 5: %+v", d)
	}

	low := llm.NewMock(&llm.Response{Text: `{"speak":false,"priority":0,"reason":"not mine"}`, StopReason: llm.StopEndTurn})
	a.Provider = low
	d, err = a.DecideSpeak(context.Background(), room, "[user] hello")
	if err != nil {
		t.Fatal(err)
	}
	if d.Priority != 1 || d.Speak {
		t.Errorf("priority 0 must clamp to 1: %+v", d)
	}

	bad := llm.NewMock(&llm.Response{Text: "I would definitely speak", StopReason: llm.StopEndTurn})
	a.Provider = bad
	if _, err := a.DecideSpeak(context.Background(), room, "[user] hello"); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("non-JSON decision must fail with the raw text: %v", err)
	}
}
