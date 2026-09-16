package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockAnthropic emulates just enough of the Messages API for room
// agents: the speak-decision (forced "respond" tool) answers "stay
// silent"; anything else gets a canned text reply. Room personas carry
// tools, so their replies take the non-streaming Complete path.
func mockAnthropic(t *testing.T, reply string, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		var body struct {
			ToolChoice *struct {
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body.ToolChoice != nil && body.ToolChoice.Name == "respond" {
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"t1","name":"respond","input":{"speak":false,"priority":1,"reason":"mock"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"` + reply + `"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pointProvidersAtMock aims loop's env-driven model resolution at the
// mock server, so hosted rooms need no real credentials.
func pointProvidersAtMock(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
}

// A hosted room answers a tagged message: the turn runs daemon-side and
// both the user line and the agent reply land in the transcript, where
// every attached client sees them. While the turn is in flight the room
// refuses a second message.
func TestDaemonRoomSayRoundTrip(t *testing.T) {
	mock := mockAnthropic(t, "mock reply from the hosted room", 400*time.Millisecond)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)

	cl, _ := startTestServer(t, fakeLoop(t, dir), filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte(`name: demo-room
agents:
  - name: scout
    role: scout
    tools: [read_file]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := cl.HostRoom(roomYAML)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if info.Name != "demo-room" || len(info.Agents) != 1 || info.Agents[0] != "scout" {
		t.Fatalf("hosted = %+v", info)
	}

	// Hosting the same file again attaches to the live session, and the
	// transcript persists across attaches.
	if info2, err := cl.HostRoom(roomYAML); err != nil || info2.Name != "demo-room" {
		t.Fatalf("re-host = %+v, %v", info2, err)
	}

	if err := cl.Say("demo-room", "@scout report status"); err != nil {
		t.Fatalf("say: %v", err)
	}

	// The turn is in flight: the room refuses a second message.
	var busyRejected bool
	for i := 0; i < 20; i++ {
		if detail, ok, _ := cl.Room("demo-room"); ok && detail.Busy {
			if err := cl.Say("demo-room", "@scout again"); err != nil && strings.Contains(err.Error(), "one message at a time") {
				busyRejected = true
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !busyRejected {
		t.Error("a second message during a turn must be refused")
	}

	// Both sides of the exchange land in the transcript.
	deadline := time.Now().Add(10 * time.Second)
	var lines []RoomLine
	for time.Now().Before(deadline) {
		lines, err = cl.RoomTranscript("demo-room", 0)
		if err == nil && len(lines) >= 2 && lines[1].Text == "mock reply from the hosted room" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || len(lines) < 2 {
		t.Fatalf("transcript = %+v (%v)", lines, err)
	}
	if lines[0].From != "user" || lines[0].Text != "@scout report status" {
		t.Errorf("line 0 = %+v", lines[0])
	}
	if lines[1].From != "scout" {
		t.Errorf("line 1 = %+v", lines[1])
	}
}

// A room run lands in the room's own context: progress and the gate go
// to the transcript, and the answer reaches the child's stdin through
// the room endpoint — the same room, drivable from any client.
func TestDaemonRoomRunPipeline(t *testing.T) {
	mock := mockAnthropic(t, "unused", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)
	fake := fakeLoop(t, dir)

	cl, _ := startTestServer(t, fake, filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte(`name: pipeline-room
agents:
  - name: scout
    role: scout
    tools: [read_file]
pipelines:
  - name: demo
    file: `+fake+`
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatal(err)
	}

	if _, err := cl.RoomRun("pipeline-room", "demo", "", nil); err != nil {
		t.Fatalf("room run: %v", err)
	}

	// The gate surfaces as a transcript line, attributed to the alias.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lines, _ := cl.RoomTranscript("pipeline-room", 0)
		joined := ""
		for _, ln := range lines {
			joined += ln.From + "|" + ln.Text + "\n"
		}
		if strings.Contains(joined, "[approval needed]") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := cl.RoomApprove("pipeline-room", "", "yes"); err != nil {
		t.Fatalf("room approve: %v", err)
	}

	// The child received the answer, and the run's completion line
	// closes the loop in the transcript.
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "answer.txt"))
		return err == nil && strings.Contains(string(data), "answered:yes")
	}, "the child to receive the answer")
	waitFor(t, 10*time.Second, func() bool {
		lines, _ := cl.RoomTranscript("pipeline-room", 0)
		for _, ln := range lines {
			if strings.Contains(ln.Text, "completed — run") {
				return true
			}
		}
		return false
	}, "the completion line to reach the transcript")
}

// Rooms are listed alphabetically by name: the dashboard re-renders
// the list every few seconds, and map iteration would shuffle it.
func TestRoomsListedAlphabetically(t *testing.T) {
	mock := mockAnthropic(t, "unused — no messages sent", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)

	cl, _ := startTestServer(t, fakeLoop(t, dir), filepath.Join(dir, "runs"))

	// Host in deliberately non-alphabetical order.
	for _, name := range []string{"war-room", "alpha-room", "mid-room"} {
		p := filepath.Join(dir, name+".yaml")
		yaml := "name: " + name + "\nagents:\n  - name: scout\n    role: scout\n"
		if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.HostRoom(p); err != nil {
			t.Fatalf("host %s: %v", name, err)
		}
	}

	// The map shuffles per call; several listings must all be sorted.
	for i := 0; i < 5; i++ {
		rooms, err := cl.Rooms()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rooms {
			got = append(got, r.Name)
		}
		want := []string{"alpha-room", "mid-room", "war-room"}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("rooms = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
