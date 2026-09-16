package webui

import (
	"strings"
	"testing"
	"time"

	"github.com/shafi-/loop/internal/daemon"
)

// Chat text is LLM/user output rendered as markdown: models' formatting
// must become real markup, and raw HTML must never pass through.
func TestRenderMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, in, want, not string }{
		{"bold", "**act now**", "<strong>act now</strong>", "**"},
		{"italic", "*maybe*", "<em>maybe</em>", "*maybe*"},
		{"hard wrap", "line one\nline two", "line one<br>", ""},
		{"fenced code", "```go\nx := 1\n```", "<pre><code", ""},
		{"raw html omitted", "<script>alert(1)</script>", "raw HTML omitted", "<script>"},
		{"list", "- a\n- b", "<ul>\n<li>a</li>\n<li>b</li>\n</ul>", ""},
		{"link", "[docs](https://x.y)", `href="https://x.y"`, ""},
		{"table", "| a | b |\n|---|---|\n| 1 | 2 |", "<table>", ""},
	} {
		got := renderMarkdown(tc.in)
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: renderMarkdown(%q) = %q, want it to contain %q", tc.name, tc.in, got, tc.want)
		}
		if tc.not != "" && strings.Contains(got, tc.not) {
			t.Errorf("%s: renderMarkdown(%q) = %q, must not contain %q", tc.name, tc.in, got, tc.not)
		}
	}
}

// Script injection survives no path: raw HTML is dropped entirely, even
// inside markdown constructs, while real markdown still renders.
func TestRenderMarkdownNeverInjectsHTML(t *testing.T) {
	got := renderMarkdown("**bold** <img src=x onerror=alert(1)> <a href=\"javascript:evil()\">x</a>")
	if strings.Contains(got, "<img") || strings.Contains(got, "onerror") || strings.Contains(got, "javascript:") {
		t.Errorf("raw html passed through: %q", got)
	}
	if !strings.Contains(got, "<strong>bold</strong>") {
		t.Errorf("markdown around the dropped html broke: %q", got)
	}
}

// Pipeline chatter collapses to two lines per run: the start and the
// latest status (live while in flight, final once it ends). Repeated
// runs of one pipeline keep their own pairs, and gate asks — decision
// points — never collapse away.
func TestChatViewCollapsesPipelineChatter(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "respond", Text: "▸ run 77 starting: respond (5 stages)"},
		{Seq: 2, From: "respond", Text: "▸ → triage (tool)"},
		{Seq: 3, From: "respond", Text: "▸ → approve (human)"},
		{Seq: 4, From: "respond", Text: "[approval needed] Approve the mitigation? — reply yes, no"},
		{Seq: 5, From: "respond", Text: "▸ → route (router)"},
		{Seq: 6, From: "respond", Text: "▸ → mitigate (tool)"},
		{Seq: 7, From: "respond", Text: "▸ completed — run 77"},
		{Seq: 8, From: "respond", Text: "▸ run 78 starting: respond (5 stages)"},
		{Seq: 9, From: "respond", Text: "▸ → triage (tool)"},
		{Seq: 10, From: "respond", Text: "▸ failed — run 78"},
	}
	chat := ChatView(lines, []string{"commander"}, nil)
	var runs, gates []ChatMsg
	for _, m := range chat {
		if m.Kind == "run" {
			runs = append(runs, m)
		}
		if m.Kind == "gate" {
			gates = append(gates, m)
		}
	}
	if len(runs) != 4 {
		t.Fatalf("run lines = %d, want two pairs (start + final per run): %+v", len(runs), runs)
	}
	want := []struct{ start, final string }{
		{"run 77 starting", "completed — run 77"},
		{"run 78 starting", "failed — run 78"},
	}
	for i, w := range want {
		if !strings.Contains(runs[2*i].Text, w.start) || !strings.Contains(runs[2*i+1].Text, w.final) {
			t.Errorf("pair %d = [%q, %q], want [%s, %s]", i, runs[2*i].Text, runs[2*i+1].Text, w.start, w.final)
		}
	}
	if len(gates) != 1 {
		t.Errorf("the gate ask must survive the collapse: %+v", gates)
	}
	for _, m := range runs {
		if m.Live {
			t.Error("finished runs are not live")
		}
	}
}

// While a run is still going, its latest line is the live status pill.
func TestChatViewMarksLiveRunStatus(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "respond", Text: "▸ run 78 starting: respond (5 stages)"},
		{Seq: 2, From: "respond", Text: "▸ → triage (tool)"},
		{Seq: 3, From: "respond", Text: "▸ → mitigate (tool)"},
	}
	runs := []daemon.RunInfo{{RunID: "78", Alias: "respond", Pipeline: "respond", Alive: true, Phase: "running"}}
	chat := ChatView(lines, []string{"commander"}, runs)
	var runMsgs []ChatMsg
	for _, m := range chat {
		if m.Kind == "run" {
			runMsgs = append(runMsgs, m)
		}
	}
	if len(runMsgs) != 2 {
		t.Fatalf("run lines = %d, want start + live: %+v", len(runMsgs), runMsgs)
	}
	if runMsgs[0].Live {
		t.Error("the start line is not the live one")
	}
	if !runMsgs[1].Live {
		t.Error("the newest line of an active run must be marked live")
	}
}

// Avatars and accents key off a stable per-name hue.
func TestHueAndInitial(t *testing.T) {
	if hue("scout") != hue("scout") {
		t.Error("hue must be deterministic")
	}
	if hue("scout") < 0 || hue("scout") > 359 {
		t.Errorf("hue out of range: %d", hue("scout"))
	}
	for _, tc := range [][2]string{{"scout", "S"}, {"comms", "C"}, {"über", "Ü"}, {"", "?"}} {
		if got := initial(tc[0]); got != tc[1] {
			t.Errorf("initial(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// ChatView renders bubbles as markdown and groups consecutive messages
// from one author: avatar + header once per group, broken by anything
// the author didn't say (event lines) or a different author.
func TestChatViewMarkdownAndGrouping(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 30, 0, 0, time.UTC)
	lines := []daemon.RoomLine{
		{Seq: 1, From: "user", Text: "status **now**", At: at},
		{Seq: 2, From: "scout", Text: "first point", At: at},
		{Seq: 3, From: "scout", Text: "second — see `state.json`", At: at},
		{Seq: 4, From: "deliver", Text: "▸ → ship (tool)", At: at},
		{Seq: 5, From: "scout", Text: "third point", At: at},
	}
	chat := ChatView(lines, []string{"scout"}, nil)

	if got := string(chat[0].HTML); !strings.Contains(got, "<strong>now</strong>") {
		t.Errorf("user markdown not rendered: %q", got)
	}
	if chat[0].At != at {
		t.Errorf("timestamp dropped: %v", chat[0].At)
	}
	// scout's two consecutive replies group; the run event breaks the
	// group, so the reply after it starts a new one.
	if chat[1].Cont || !chat[2].Cont {
		t.Errorf("grouping wrong: first=%v second=%v", chat[1].Cont, chat[2].Cont)
	}
	if got := string(chat[2].HTML); !strings.Contains(got, "<code>state.json</code>") {
		t.Errorf("agent markdown not rendered: %q", got)
	}
	if chat[4].Cont {
		t.Error("an event line between replies must break the group")
	}
}
