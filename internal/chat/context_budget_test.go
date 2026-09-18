package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// The conversation string is byte-budgeted: newest messages win, a
// message larger than the whole budget clips instead of vanishing, and
// dropped history is announced.
func TestBuildConversationByteBudget(t *testing.T) {
	tr, err := OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		if err := tr.Append("user", strings.Repeat("x", 900)+fmt.Sprintf(" m%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// A tight budget: only the newest messages fit.
	s := tr.BuildConversation(50, 2000)
	if !strings.Contains(s, omittedMarker) {
		t.Error("dropped history must be announced")
	}
	if strings.Contains(s, "m001") || !strings.Contains(s, "m030") {
		t.Errorf("newest must win:\n%.200s…\n%.200s", s[:200], s[len(s)-200:])
	}
	if len(s) > 2000+512 { // budget + marker/clip slack
		t.Errorf("conversation len = %d, budget 2000", len(s))
	}
	// A generous budget keeps everything with no marker.
	if s2 := tr.BuildConversation(50, 1<<20); strings.Contains(s2, omittedMarker) {
		t.Error("generous budget must not drop history")
	}
	// A message larger than the whole budget drops when newer messages
	// exist (newest wins); alone, it clips instead of vanishing.
	tr2, _ := OpenTranscript(t.TempDir())
	if err := tr2.Append("user", strings.Repeat("y", 20_000)); err != nil {
		t.Fatal(err)
	}
	if err := tr2.Append("user", "small tail"); err != nil {
		t.Fatal(err)
	}
	s3 := tr2.BuildConversation(10, 4000)
	if !strings.Contains(s3, "small tail") || !strings.Contains(s3, omittedMarker) {
		t.Errorf("the newer message must survive the over-budget older one:\n%.300s", s3)
	}
	tr3, _ := OpenTranscript(t.TempDir())
	if err := tr3.Append("user", strings.Repeat("z", 20_000)+"THEEND"); err != nil {
		t.Fatal(err)
	}
	s4 := tr3.BuildConversation(10, 4000)
	if !strings.Contains(s4, "[message clipped]") || !strings.HasPrefix(s4, "[user] zzz") {
		t.Errorf("a lone over-budget message must clip, not vanish:\n%.200s", s4)
	}
}

// Decisions see a tighter context than replies — recency, not depth —
// and the CEO's new message is not embedded twice.
func TestDecisionsUseTighterContext(t *testing.T) {
	tr, err := OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		if err := tr.Append("user", fmt.Sprintf("filler %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	personas := []config.Persona{
		{Name: "ceo", System: "You are the CEO."},
		{Name: "cfo", System: "You are the CFO."},
	}
	decide := llm.NewMock(&llm.Response{Text: `{"speak":false,"priority":1,"reason":"not mine"}`})
	agents := []*agent.Agent{
		{Persona: personas[0], Provider: llm.NewMock(&llm.Response{Text: "hi"})},
		{Persona: personas[1], Provider: decide},
	}
	r := NewRoom(config.Room{Name: "demo", Agents: personas}, agents, tr)
	if err := r.Say(context.Background(), "@ceo decide-check", noopUI{}); err != nil {
		t.Fatal(err)
	}
	// @ceo was tagged: the cfo ran a decision with the small window.
	reqs := decide.Requests()
	if len(reqs) == 0 {
		t.Fatal("no decision call recorded")
	}
	content := reqs[0].Messages[0].Content
	if strings.Contains(content, "filler 1\n") {
		t.Error("decision saw messages beyond the decision window")
	}
	if !strings.Contains(content, "decide-check") {
		t.Error("decision prompt missing the new message (its last line)")
	}
	if strings.Contains(content, "=== New message from the CEO ===") {
		t.Error("the new message must not be embedded twice")
	}
}
