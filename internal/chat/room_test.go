package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// recorderUI captures everything the room reports.
type recorderUI struct {
	replies []string
	started []string
	seen    [][]string
	capped  []string
	notices []string
}

func (r *recorderUI) AgentReplyStart(name string) { r.started = append(r.started, name) }
func (r *recorderUI) AgentTextDelta(name, delta string) {
	r.replies = append(r.replies, name+":"+delta)
}
func (r *recorderUI) AgentsSeen(names []string) { r.seen = append(r.seen, names) }
func (r *recorderUI) AgentCapped(name string, priority int) {
	r.capped = append(r.capped, fmt.Sprintf("%s:%d", name, priority))
}
func (r *recorderUI) Notice(format string, args ...any) {
	r.notices = append(r.notices, fmt.Sprintf(format, args...))
}

// decisionJSON renders a scripted decision as the provider would return it.
func decisionJSON(speak bool, priority int, reason string) *llm.Response {
	return &llm.Response{Text: fmt.Sprintf(`{"speak":%t,"priority":%d,"reason":%q}`, speak, priority, reason)}
}

func testPersona(name, model string) config.Persona {
	return config.Persona{
		Name:   name,
		Role:   strings.ToUpper(name[:1]) + name[1:],
		System: "You are " + name + ".",
		Model:  &config.ModelConfig{Provider: config.ProviderOpenAI, Model: model},
	}
}

func newTestRoom(t *testing.T, personas []config.Persona, mocks map[string]llm.Provider) (*Room, *Transcript) {
	t.Helper()
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	var agents []*agent.Agent
	for _, p := range personas {
		agents = append(agents, &agent.Agent{Persona: p, Provider: mocks[p.Name]})
	}
	cfg := config.Room{Name: "test", Agents: personas}
	return NewRoom(cfg, agents, tr), tr
}

func TestTaggedParsing(t *testing.T) {
	known := map[string]bool{"ceo": true, "cfo": true, "architect": true}
	got := tagged("@cfo and @ceo — @cfo again, @nobody is unknown", known)
	if len(got) != 2 || got[0] != "cfo" || got[1] != "ceo" {
		t.Errorf("tagged = %v (want [cfo ceo] in mention order)", got)
	}
}

func TestDecisionConfidence(t *testing.T) {
	cases := []struct {
		prio int
		want float64
	}{
		{1, 0}, {3, 0.5}, {4, 0.75}, {5, 1},
	}
	for _, tc := range cases {
		if got := (agent.Decision{Priority: tc.prio}).Confidence(); got != tc.want {
			t.Errorf("priority %d confidence = %v, want %v", tc.prio, got, tc.want)
		}
	}
}

func TestTranscriptWindowAndPersistence(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := tr.Append("user", fmt.Sprintf("msg %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(tr.Window(2)); got != 2 {
		t.Fatalf("window = %d messages", got)
	}
	if !strings.Contains(Render(tr.Window(5)), "[user] msg 4") {
		t.Errorf("render = %q", Render(tr.Window(5)))
	}
	// Reload from disk: persistence round trip.
	reloaded, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Messages) != 5 || reloaded.Messages[4].Text != "msg 4" {
		t.Errorf("reload = %d messages, last %q", len(reloaded.Messages), reloaded.Messages[4].Text)
	}
}

func TestTaggedReplyHappensBeforeObserverDecisions(t *testing.T) {
	// The cofounder's rule: observers decide only after tagged agents spoke,
	// so their decision sees the full turn. We prove it by checking that the
	// observer's decision-call prompt contains the tagged agent's reply.
	ceoMock := llm.NewMock(&llm.Response{Text: "we ship it friday"})
	cfoMock := llm.NewMock(decisionJSON(false, 1, "no financial stakes"))
	r, _ := newTestRoom(t, []config.Persona{testPersona("ceo", "m1"), testPersona("cfo", "m2")},
		map[string]llm.Provider{"ceo": ceoMock, "cfo": cfoMock})
	ui := &recorderUI{}

	if err := r.Say(context.Background(), "@ceo when do we ship?", ui); err != nil {
		t.Fatal(err)
	}
	if len(ceoMock.Requests()) != 1 {
		t.Fatalf("ceo calls = %d", len(ceoMock.Requests()))
	}
	if len(cfoMock.Requests()) != 1 {
		t.Fatalf("cfo should have exactly one decision call, has %d", len(cfoMock.Requests()))
	}
	decisionReq := cfoMock.Requests()[0]
	if !strings.Contains(decisionReq.Messages[0].Content, "we ship it friday") {
		t.Error("observer decided BEFORE the tagged reply existed in the conversation")
	}
	if len(ui.seen) != 1 || len(ui.seen[0]) != 1 || ui.seen[0][0] != "cfo" {
		t.Errorf("seen receipts = %v (want [cfo])", ui.seen)
	}
	// The decision schema must be set (structured output path).
	if decisionReq.ResponseSchema == nil {
		t.Error("decision call must use structured output")
	}
}

func TestSpontaneousReplyAndSeenReceipts(t *testing.T) {
	// Untagged agents make two calls: decision first, then reply when
	// qualified. Script both explicitly — a single-response script would
	// leak the decision JSON into the reply.
	ceoMock := llm.NewMock(decisionJSON(true, 5, "leadership call"), &llm.Response{Text: "the ceo speaks"})
	cfoMock := llm.NewMock(decisionJSON(false, 1, "no financial stakes"))
	archMock := llm.NewMock(decisionJSON(true, 4, "architecture relevant"), &llm.Response{Text: "architect agrees"})
	r, tr := newTestRoom(t,
		[]config.Persona{testPersona("ceo", "m1"), testPersona("cfo", "m2"), testPersona("architect", "m3")},
		map[string]llm.Provider{"ceo": ceoMock, "cfo": cfoMock, "architect": archMock})
	ui := &recorderUI{}

	if err := r.Say(context.Background(), "should we build a data warehouse?", ui); err != nil {
		t.Fatal(err)
	}
	// ceo qualified (priority 5, threshold 0.6 → confidence 1.0); architect
	// qualified (0.75); cfo silent.
	found := map[string]bool{}
	for _, s := range ui.replies {
		found[strings.SplitN(s, ":", 2)[0]] = true
	}
	if !found["ceo"] || !found["architect"] {
		t.Errorf("qualified agents did not speak: %v", ui.replies)
	}
	if len(ui.seen) != 1 || len(ui.seen[0]) != 1 || ui.seen[0][0] != "cfo" {
		t.Errorf("seen receipts = %v (want [cfo])", ui.seen)
	}
	// Transcript has user + 2 replies, and replies carry reply text —
	// never a leaked decision payload.
	if len(tr.Messages) != 3 {
		t.Fatalf("transcript = %d messages", len(tr.Messages))
	}
	if tr.Messages[1].Text != "the ceo speaks" || tr.Messages[2].Text != "architect agrees" {
		t.Errorf("reply texts = %q, %q", tr.Messages[1].Text, tr.Messages[2].Text)
	}
	// CEO replied before architect (priority order 5 > 4).
	if tr.Messages[1].From != "ceo" || tr.Messages[2].From != "architect" {
		t.Errorf("reply order = [%s, %s]", tr.Messages[1].From, tr.Messages[2].From)
	}
}

func TestAntiPileOnCap(t *testing.T) {
	a1 := llm.NewMock(decisionJSON(true, 5, "critical"), &llm.Response{Text: "p1 reply"})
	a2 := llm.NewMock(decisionJSON(true, 5, "also critical"), &llm.Response{Text: "p2 reply"})
	a3 := llm.NewMock(decisionJSON(true, 5, "me too"), &llm.Response{Text: "p3 reply"})
	personas := []config.Persona{testPersona("p1", "m"), testPersona("p2", "m"), testPersona("p3", "m")}
	r, tr := newTestRoom(t, personas, map[string]llm.Provider{"p1": a1, "p2": a2, "p3": a3})
	ui := &recorderUI{}

	if err := r.Say(context.Background(), "untagged message", ui); err != nil {
		t.Fatal(err)
	}
	spoke := map[string]int{}
	for _, m := range tr.Messages {
		if m.From != "user" {
			spoke[m.From]++
		}
	}
	total := 0
	for _, n := range spoke {
		total += n
	}
	if total != 2 {
		t.Errorf("max_spontaneous_replies=2 but %d agents spoke (%v)", total, spoke)
	}
	if len(ui.capped) != 1 {
		t.Errorf("capped reports = %v", ui.capped)
	}
	// Spoken replies carry reply text, not leaked decision JSON.
	for _, m := range tr.Messages {
		if m.From != "user" && strings.HasPrefix(m.Text, "{") {
			t.Errorf("%s replied with a decision payload: %q", m.From, m.Text)
		}
	}
}

func TestObserverFailureDegradesToSilence(t *testing.T) {
	dead := failingObserver{}
	p := testPersona("ceo", "m")
	r, _ := newTestRoom(t, []config.Persona{p, testPersona("broken", "m")},
		map[string]llm.Provider{"ceo": llm.NewMock(decisionJSON(false, 1, "")), "broken": dead})
	ui := &recorderUI{}
	if err := r.Say(context.Background(), "hello", ui); err != nil {
		t.Fatalf("observer failure must not fail the room: %v", err)
	}
	if len(ui.notices) == 0 {
		t.Error("observer failure should produce a notice")
	}
}

type failingObserver struct{}

func (failingObserver) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, fmt.Errorf("provider unreachable")
}
func (failingObserver) Stream(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, fmt.Errorf("provider unreachable")
}

func TestTranscriptPathReported(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(filepath.Join(dir, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.path == "" || filepath.Base(tr.path) != "transcript.jsonl" {
		t.Errorf("transcript path = %q", tr.path)
	}
}
