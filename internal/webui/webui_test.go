package webui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shafi-/loop/internal/daemon"
)

func ev(typ string, payload string) daemon.EventLine {
	return daemon.EventLine{Seq: 1, Type: typ, Event: json.RawMessage(payload)}
}

// The timeline classification is the UI's content logic: events in,
// display rows out — no server, no daemon needed.
func TestTimelineClassification(t *testing.T) {
	events := []daemon.EventLine{
		ev("run_started", `{"pipeline":"demo"}`),
		ev("stage_started", `{"stage":"draft","stage_type":"tool"}`),
		ev("router_decision", `{"stage":"route","next":"ship"}`),
		ev("human_prompt", `{"stage":"approval","text":"Ship it?"}`),
		ev("human_answer", `{"stage":"approval","answer":"yes"}`),
		ev("human_intent", `{"stage":"approval","intent":"yes","via":"vocabulary"}`),
		ev("stage_failed", `{"stage":"compile","error":"boom"}`),
		ev("narration", `{"text":"almost there"}`),
		ev("executor_event", `{"kind":"tool_call","tool":"write_file","detail":"plans/x.md"}`),
		ev("run_completed", `{"steps":3}`),
		ev("provider_detail", `{"detail":"not shown"}`), // unknown kinds are skipped
	}
	rows := Timeline(events)
	want := []struct{ class, head string }{
		{"started", "▶ pipeline demo"},
		{"stage", "→ draft (tool)"},
		{"router", "⤷ route routed to ship"},
		{"gate", "✋ input needed — approval"},
		{"answer", "↩ answered"},
		{"intent intent-yes", "understood as: yes (vocabulary)"},
		{"failure", "✗ compile failed"},
		{"narration", "almost there"},
		{"executor", "🔧 tool_call: write_file"},
		{"terminal ok", "✓ run complete (3 steps)"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Class != w.class || rows[i].Head != w.head {
			t.Errorf("row %d = {%s %q}, want {%s %q}", i, rows[i].Class, rows[i].Head, w.class, w.head)
		}
	}
	// Long payloads render as collapsible blocks, not inline spans.
	long := Timeline([]daemon.EventLine{ev("human_prompt", `{"stage":"a","text":"`+strings.Repeat("x", 200)+`"}`)})
	if !long[0].LongBlk {
		t.Error("a long gate prompt must be marked LongBlk")
	}
}

// Consecutive tool notices from one agent collapse into a single
// bubble carrying the newest text and the attempt count — a flailing
// agent must not flood the conversation.
func TestChatViewCollapsesToolNoise(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "user", Text: "status?"},
		{Seq: 2, From: "scout", Text: "[tool] read run.json — failed: not found"},
		{Seq: 3, From: "scout", Text: "[tool] read log.md — failed: not found"},
		{Seq: 4, From: "scout", Text: "[tool] read README.md — failed: not found"},
		{Seq: 5, From: "scout", Text: "[tool] read .loop/runs/2026/state.json"},
		{Seq: 6, From: "scout", Text: "done — state: completed"},
	}
	chat := ChatView(lines, []string{"scout"}, nil)
	var tools []ChatMsg
	for _, m := range chat {
		if m.Kind == "tool" {
			tools = append(tools, m)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("tool bubbles = %d, want 1 collapsed: %+v", len(tools), tools)
	}
	if tools[0].Count != 4 {
		t.Errorf("count = %d, want 4", tools[0].Count)
	}
	if !strings.Contains(tools[0].Text, "state.json") {
		t.Errorf("collapsed bubble must carry the newest attempt: %q", tools[0].Text)
	}
}

// ChatView sorts transcript lines into chat shapes: user bubbles, agent
// bubbles, tool notices, run events, gate asks.
func TestChatViewClassification(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "user", Text: "@scout report"},
		{Seq: 2, From: "scout", Text: "on it"},
		{Seq: 3, From: "scout", Text: "[tool] wrote plans/x.md"},
		{Seq: 4, From: "deliver", Text: "▸ run 99 starting: demo (2 stages)"},
		{Seq: 5, From: "deliver", Text: "[approval needed] Ship it? — reply yes, no, or your change requests"},
		{Seq: 6, From: "system", Text: "turn failed: scout: context canceled"},
	}
	chat := ChatView(lines, []string{"scout"}, nil)
	want := []struct{ kind, from string }{
		{"user", "you"},
		{"agent", "scout"},
		{"tool", "scout"},
		{"run", "deliver"},
		{"gate", "deliver"},
		{"system", ""},
	}
	if len(chat) != len(want) {
		t.Fatalf("chat = %d msgs, want %d: %+v", len(chat), len(want), chat)
	}
	for i, w := range want {
		if chat[i].Kind != w.kind || chat[i].From != w.from {
			t.Errorf("msg %d = {%s %q}, want {%s %q}", i, chat[i].Kind, chat[i].From, w.kind, w.from)
		}
	}
	// The gate ask drops its bracket marker and the progress bullet.
	if strings.Contains(chat[4].Text, "approval needed") || strings.Contains(chat[3].Text, "▸") {
		t.Errorf("run lines carry markup: %+v %+v", chat[3], chat[4])
	}
}

// fakeLoop is the daemon's child stand-in: exact stderr shapes, one
// stdin answer, receipt file.
func fakeLoop(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-loop")
	content := `#!/bin/sh
echo "run fake starting: demo (2 stages)" >&2
echo "── your input needed ──────────────────────" >&2
echo "Approve the draft?" >&2
echo "(/pause · /quit · /exit pause the run — resumable)" >&2
read answer
echo "✓ run fake complete (2 steps)" >&2
echo "answered:$answer" > "` + dir + `/answer.txt"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// newUI wires a webui over a real daemon (fake child) on temp sockets.
func newUI(t *testing.T, dir string) (*daemon.Client, http.Handler) {
	t.Helper()
	script := fakeLoop(t, dir)
	srv := daemon.New("test-ui", filepath.Join(dir, "runs"), script)
	base, err := os.MkdirTemp("", "loopui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	sock := filepath.Join(base, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln)
	t.Cleanup(cancel)

	cl, err := daemon.Dial(sock)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	handler, err := New("ui-test", sock)
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	return cl, handler
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The full UI flow over a live daemon: dashboard → submit through the
// form endpoint → gate rendered → answered through the form endpoint →
// child received the answer. The /api proxy passes through to the daemon.
func TestWebUIFlowOverLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	cl, handler := newUI(t, dir)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// Dashboard renders its shell.
	if resp, body := get(t, ts.URL+"/"); resp.StatusCode != 200 || !strings.Contains(body, "start a run") {
		t.Fatalf("dashboard = %d %q", resp.StatusCode, body)
	}

	// Submit a run through the form endpoint.
	resp, body := postForm(t, ts.URL+"/runs", "file="+dir+"/fake-loop&vars=")
	if resp.StatusCode != 200 || !strings.Contains(body, "submitted") {
		t.Fatalf("submit = %d %q", resp.StatusCode, body)
	}
	runID := between(body, `href="/runs/`, `">`)

	// The gate surfaces in the timeline fragment.
	waitFor(t, func() bool {
		_, b := get(t, ts.URL+"/runs/"+runID+"/timeline")
		return strings.Contains(b, "your input needed") && strings.Contains(b, "Approve the draft?")
	}, "the gate to render in the timeline")

	// Answering through the form reaches the child's stdin.
	if resp, _ := postForm(t, ts.URL+"/runs/"+runID+"/answer", "quick=yes"); resp.StatusCode != 200 {
		t.Fatalf("answer = %d (runID %q)", resp.StatusCode, runID)
	}
	waitFor(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "answer.txt"))
		return err == nil && strings.Contains(string(data), "answered:yes")
	}, "the child to receive the answer")

	// The run retires (a fake child leaves no artifacts, so it leaves
	// both lists — history rendering needs real run dirs, covered by the
	// daemon's real-binary test); the fragment still renders.
	waitFor(t, func() bool {
		runs, err := cl.Runs(false)
		return err == nil && len(runs) == 0
	}, "the finished run to retire")
	if _, b := get(t, ts.URL+"/frag/runs"); !strings.Contains(b, "active") {
		t.Errorf("fragment = %q", b)
	}

	// The /api proxy reaches the daemon.
	resp, body = get(t, ts.URL+"/api/ping")
	if resp.StatusCode != 200 || !strings.Contains(body, `"version":"test-ui"`) {
		t.Errorf("proxied ping = %d %q", resp.StatusCode, body)
	}

	// Rooms are not part of the runs poll anymore: they preload once
	// via their own fragment, and the workspace pickers re-render on
	// demand for the refresh buttons.
	if _, b := get(t, ts.URL+"/frag/rooms"); !strings.Contains(b, "no rooms hosted") {
		t.Errorf("frag/rooms = %q", b)
	}
	if resp, b := get(t, ts.URL+"/frag/workspace?kind=pipelines"); resp.StatusCode != 200 {
		t.Errorf("frag/workspace = %d %q", resp.StatusCode, b)
	}
	if _, b := get(t, ts.URL+"/"); !strings.Contains(b, `hx-get="/frag/rooms" hx-trigger="load"`) {
		t.Errorf("dashboard missing the rooms preload")
	}

	// Unknown run ids render 404s, not stack traces.
	if resp, _ = get(t, ts.URL+"/runs/9999"); resp.StatusCode != 404 {
		t.Errorf("unknown run = %d", resp.StatusCode)
	}
}

// The page head carries the identity: favicon set, theme color, and a
// title (plus nav brand) that names the workspace so project tabs are
// tellable apart.
func TestHeadIdentity(t *testing.T) {
	_, handler := newUI(t, t.TempDir())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	_, body := get(t, ts.URL+"/")
	for _, want := range []string{
		`rel="icon" type="image/svg+xml" href="/assets/favicon.svg?v=3"`,
		`rel="alternate icon" type="image/png" sizes="32x32" href="/assets/favicon-32.png?v=3"`,
		`rel="apple-touch-icon" href="/assets/apple-touch-icon.png?v=3"`,
		`<meta name="theme-color" content="#0d1117">`,
		`<title>webui · loop — runs</title>`, // daemon CWD is this package's dir
		`<span class="mark"></span>loop`,     // the bold-circle brand mark
		`<a href="https://shafi-.github.io/loop/" target="_blank" rel="noreferrer">docs</a>`,
		`<a class="gh" href="https://github.com/shafi-/loop"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard head missing %q", want)
		}
	}
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func postForm(t *testing.T, url, form string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
