package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// diveRoom builds a room where the architect has tools; every provider
// is a mock keyed by agent name. First response = the dive decision,
// second = the streamed reply.
func diveRoom(t *testing.T, startDive func(ctx context.Context, spec DiveSpec) error) (*Room, *recorderUI, *llm.Mock) {
	t.Helper()
	archMock := llm.NewMock(
		&llm.Response{Text: `{"deep_dive":true,"dive_topic":"session invalidation in auth/","reason":"notes are thin"}`},
		&llm.Response{Text: "My read: sessions leak. I could verify deeply."},
	)
	personas := []config.Persona{
		testPersona("architect", "architect"),
		testPersona("cfo", "cfo"),
	}
	archPersona := personas[0]
	archPersona.Tools = []string{"read_file", "write_file"}
	agents := []*agent.Agent{
		{Persona: archPersona, Provider: archMock, CWD: t.TempDir()},
		{Persona: personas[1], Provider: llm.NewMock(&llm.Response{Text: `{"speak":false,"priority":1,"reason":"","deep_dive":false,"dive_topic":""}`})},
	}
	tr, err := OpenTranscript(filepath.Join(t.TempDir(), "dev"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Room{Name: "dev", Agents: personas}
	r := NewRoom(cfg, agents, tr)
	r.StartDive = startDive
	return r, &recorderUI{}, archMock
}

// A tagged reply by a tool-using agent whose decision flags a dive is
// followed by the deterministic offer line, and the offer is recorded.
func TestTaggedReplyOffersDive(t *testing.T) {
	var launched DiveSpec
	r, ui, _ := diveRoom(t, func(_ context.Context, spec DiveSpec) error { launched = spec; return nil })
	if err := r.Say(context.Background(), "@architect why do sessions leak?", ui); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range r.Transcript.Messages {
		if strings.HasPrefix(m.Text, diveOfferMarker) && strings.Contains(m.Text, "@architect") &&
			strings.Contains(m.Text, "session invalidation in auth/") && strings.Contains(m.Text, "/deepdive @architect") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no offer line:\n%v", r.Transcript.Messages)
	}
	if _, ok := r.loadDives()["architect"]; !ok {
		t.Error("offer not recorded for /deepdive topic memory")
	}
	if launched.Agent != "" {
		t.Error("dive launched without consent")
	}
}

// The CTA (or a typed line): /deepdive without a topic reuses the last
// offered one and hands the host a complete spec.
func TestDeepDiveCommandLaunchesWithRememberedTopic(t *testing.T) {
	var got DiveSpec
	r, ui, _ := diveRoom(t, func(_ context.Context, spec DiveSpec) error { got = spec; return nil })
	if err := r.Say(context.Background(), "@architect why do sessions leak?", ui); err != nil {
		t.Fatal(err)
	}
	if err := r.Say(context.Background(), "/deepdive @architect", ui); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "architect" || got.Topic != "session invalidation in auth/" {
		t.Errorf("spec = %+v", got)
	}
	if got.System == "" || len(got.Tools) == 0 || got.Question == "" {
		t.Errorf("spec missing grounding: %+v", got)
	}
	// The launch and the report contract are transcript lines.
	joined := transcriptText(r)
	if !strings.Contains(joined, "▶ @architect deep dive started") {
		t.Errorf("no launch line:\n%s", joined)
	}
}

// An explicit topic wins over the remembered one.
func TestDeepDiveCommandExplicitTopicWins(t *testing.T) {
	var got DiveSpec
	r, ui, _ := diveRoom(t, func(_ context.Context, spec DiveSpec) error { got = spec; return nil })
	if err := r.Say(context.Background(), "/deepdive @architect where is rate limiting enforced", ui); err != nil {
		t.Fatal(err)
	}
	if got.Topic != "where is rate limiting enforced" {
		t.Errorf("topic = %q", got.Topic)
	}
}

// A persona without tools cannot dive, with or without an offer.
func TestDeepDiveRefusesToollessAgent(t *testing.T) {
	r, ui, _ := diveRoom(t, func(_ context.Context, _ DiveSpec) error { return nil })
	err := r.Say(context.Background(), "/deepdive @cfo costs", ui)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcriptText(r), "no tools to deep-dive with") {
		t.Error("expected the toolless refusal")
	}
}

// No host hook: /deepdive explains instead of failing silently.
func TestDeepDiveWithoutHookExplains(t *testing.T) {
	r, ui, _ := diveRoom(t, nil)
	r.StartDive = nil
	r.DiveUnavailableReason = "the agent executor is not ready — run: loop setup"
	if err := r.Say(context.Background(), "/deepdive @architect costs", ui); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcriptText(r), "loop setup") {
		t.Error("expected the availability explanation")
	}
	// And without a hook the tagged path never offers.
	if err := r.Say(context.Background(), "@architect why do sessions leak?", ui); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(transcriptText(r), diveOfferMarker) {
		t.Error("offer made although dives cannot run")
	}
}

// No topic given and no offer remembered: a usage error, not a launch.
func TestDeepDiveWithoutTopicOrOfferFails(t *testing.T) {
	r, ui, _ := diveRoom(t, func(_ context.Context, _ DiveSpec) error { return nil })
	if err := r.Say(context.Background(), "/deepdive @architect", ui); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcriptText(r), "no topic given") {
		t.Error("expected the topic usage error")
	}
}

// An observer whose speak decision flags a dive offers too — after its
// spontaneous reply.
func TestObserverDiveOffer(t *testing.T) {
	r, ui, _ := diveRoom(t, func(_ context.Context, _ DiveSpec) error { return nil })
	// The CFO speaks and offers: override its mock with a speak+dive
	// decision followed by a streamed reply, and grant it tools (no
	// tools, no offer — that's TestDeepDiveRefusesToollessAgent).
	r.Agents[1].Persona.Tools = []string{"read_file"}
	r.Agents[1].Provider = llm.NewMock(
		&llm.Response{Text: `{"speak":true,"priority":5,"reason":"mine","deep_dive":true,"dive_topic":"pricing tables"}`},
		&llm.Response{Text: "Costs matter here."},
	)
	if err := r.Say(context.Background(), "what does this feature cost us?", ui); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcriptText(r), diveOfferMarker+" @cfo") {
		t.Error("no observer offer line")
	}
}

func transcriptText(r *Room) string {
	var b strings.Builder
	for _, m := range r.Transcript.Messages {
		b.WriteString(m.From + ": " + m.Text + "\n")
	}
	return b.String()
}

// The offer state survives a reopen: topic memory is the file's job.
func TestDiveStatePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(filepath.Join(dir, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	r, ui, _ := diveRoom(t, func(_ context.Context, _ DiveSpec) error { return nil })
	r.Transcript = tr
	if err := r.Say(context.Background(), "@architect why do sessions leak?", ui); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev", "deepdive.json")); err != nil {
		t.Fatalf("deepdive.json not written: %v", err)
	}
	// Reopen: a fresh room over the same dir remembers the topic.
	tr2, err := OpenTranscript(filepath.Join(dir, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	r2 := NewRoom(config.Room{Name: "dev"}, nil, tr2)
	if got := r2.loadDives()["architect"]; got.Topic != "session invalidation in auth/" {
		t.Errorf("reloaded offer = %+v", got)
	}
}
