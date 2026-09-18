package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
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
	"github.com/shafi-/loop/internal/usage"
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

	// Answering through the form reaches the child's stdin. The body is
	// asserted too: a delivered answer must never render as a 404, no
	// matter how fast the run retires afterwards.
	if resp, body := postForm(t, ts.URL+"/runs/"+runID+"/answer", "quick=yes"); resp.StatusCode != 200 || strings.Contains(body, "404") {
		t.Fatalf("answer = %d (runID %q) body: %s", resp.StatusCode, runID, body)
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
	return postClient(t, http.DefaultClient, url, form)
}

func postClient(t *testing.T, c *http.Client, url, form string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Post(url, "application/x-www-form-urlencoded", strings.NewReader(form))
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

// The fork affordance hangs on the last message of each user/agent
// group, carrying the group's last transcript line — forking through it
// keeps the whole message.
func TestChatViewForkPoints(t *testing.T) {
	lines := []daemon.RoomLine{
		{Seq: 1, From: "user", Text: "hello"},
		{Seq: 2, From: "scout", Text: "first reply"},
		{Seq: 3, From: "scout", Text: "second reply"},
		{Seq: 4, From: "run-x", Text: "run 1 starting: demo (2 stages)"},
		{Seq: 5, From: "user", Text: "and now this"},
	}
	msgs := ChatView(lines, []string{"scout"}, nil)
	var forks []int
	for i, m := range msgs {
		if m.Fork {
			forks = append(forks, i)
		}
	}
	if len(forks) != 3 {
		t.Fatalf("fork points at %v, want one per user/agent group (3)", forks)
	}
	// The user bubble keeps through line 1; the agent group's tail keeps
	// through line 3; the final user message keeps through line 5.
	if msgs[forks[0]].Line != 1 || msgs[forks[1]].Line != 3 || msgs[forks[2]].Line != 5 {
		t.Errorf("fork lines = %d,%d,%d, want 1,3,5", msgs[forks[0]].Line, msgs[forks[1]].Line, msgs[forks[2]].Line)
	}
	if msgs[forks[1]].Text != "second reply" {
		t.Errorf("the agent fork carrier = %q, want the group's last message", msgs[forks[1]].Text)
	}
}

// The room's markup wires the fork and reset actions to the daemon.
func TestChatFragmentRotateMarkup(t *testing.T) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"phaseLabel": phaseLabel,
		"hms":        func(t time.Time) string { return t.Local().Format("15:04") },
		"hue":        hue,
		"initial":    initial,
		"human":      usage.Human,
		"md":         func(s string) template.HTML { return template.HTML(s) },
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	data := pageData{
		Room: daemon.RoomInfo{Name: "demo"},
		Chat: []ChatMsg{{Kind: "user", From: "you", HTML: "<p>hi</p>", Line: 7, Fork: true}},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "chat_fragment", data); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	for _, want := range []string{
		`hx-post="/rooms/demo/fork"`,
		`{"through":7}`,
		`hx-confirm="Fork the room here?`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("chat fragment missing %q", want)
		}
	}

	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "room", pageData{
		Room: daemon.RoomInfo{Name: "demo"},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `hx-post="/rooms/demo/reset"`) {
		t.Errorf("room page missing the reset button")
	}
}

// Typing /reset in the room's composer must rotate the conversation —
// never become a message the agents puzzle over (the bug report that
// inspired this: agents "still referred to the old transcription"
// because the composer swallowed the command as chat text).
func TestComposerResetCommand(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1") // hosting never calls it
	dir := t.TempDir()
	cl, handler := newUI(t, dir)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: demo\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatalf("host: %v", err)
	}

	// An ordinary message first, so the reset has something to clear.
	if resp, _ := postForm(t, ts.URL+"/rooms/demo/say", "text=hello there"); resp.StatusCode != 200 {
		t.Fatalf("say = %d", resp.StatusCode)
	}
	waitFor(t, func() bool {
		info, ok, _ := cl.Room("demo")
		return ok && !info.Busy
	}, "the turn to settle")

	if resp, _ := postForm(t, ts.URL+"/rooms/demo/say", "text=/reset"); resp.StatusCode != 200 {
		t.Fatalf("composer /reset = %d", resp.StatusCode)
	}
	// The rotation runs in the daemon's turn goroutine — poll for it.
	var lines []daemon.RoomLine
	waitFor(t, func() bool {
		lines, _ = cl.RoomTranscript("demo", 0)
		return len(lines) == 1 && lines[0].From == "system" && strings.Contains(lines[0].Text, "room reset")
	}, "the composer /reset to rotate the transcript")
	for _, ln := range lines {
		if ln.Text == "/reset" {
			t.Errorf("the command leaked into the transcript as a message")
		}
	}

	// /fork <n> through the composer too.
	if resp, _ := postForm(t, ts.URL+"/rooms/demo/say", "text=/fork+1"); resp.StatusCode != 200 {
		t.Fatalf("composer /fork = %d", resp.StatusCode)
	}
	waitFor(t, func() bool {
		lines, _ = cl.RoomTranscript("demo", 0)
		return len(lines) == 2 && lines[1].From == "system"
	}, "the composer /fork to rotate the transcript")
}
