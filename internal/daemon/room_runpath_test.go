package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A typed /run with a pipeline PATH (not a room alias) must start the
// run from any room client — the web composer sends plain text, so the
// verb lives in the room engine. The room owns no pipelines block at
// all; the path is the whole interface.
func TestDaemonRoomTypedRunByPath(t *testing.T) {
	mock := mockAnthropic(t, "unused", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)
	fake := fakeLoop(t, dir)

	cl, _ := startTestServer(t, fake, filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: path-room\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatal(err)
	}
	// The pipeline the user wants: a plain workspace file this room
	// does not own under any alias.
	if err := os.MkdirAll(filepath.Join(dir, ".loop/pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loop/pipelines", "adhoc.yaml"),
		[]byte("name: adhoc\nstages:\n  - id: only\n    type: tool\n    run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The requirement is stated in the room BEFORE the /run: the
	// hand-off must carry it to the pipeline. The requirement's turn
	// may still be processing (rooms take one message at a time), so
	// the /run retries until the room accepts input again.
	if err := cl.Say("path-room", "add rate limiting to the API"); err != nil {
		t.Fatalf("say: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		return cl.Say("path-room", "/run .loop/pipelines/adhoc.yaml") == nil
	}, "the room to accept the /run (the requirement's turn to finish)")

	// The start line lands in the transcript, then the run's own lines
	// (the fake child emits a gate; its label is the path's base).
	waitFor(t, 10*time.Second, func() bool {
		lines, _ := cl.RoomTranscript("path-room", 0)
		started, gated := false, false
		for _, ln := range lines {
			if strings.Contains(ln.Text, "adhoc.yaml started — run") {
				started = true
			}
			if ln.From == "adhoc" && strings.Contains(ln.Text, "[approval needed]") {
				gated = true
			}
		}
		return started && gated
	}, "the typed path run to start and gate")

	// The run received the room's context: the hand-off brief exists in
	// the room's record dir with the ask verbatim, and the child was
	// started with it as the room_context var (the fake loop dumps argv).
	handoffs, _ := filepath.Glob(filepath.Join(dir, ".loop", "rooms", "*", "handoff.md"))
	if len(handoffs) != 1 {
		t.Fatalf("expected one hand-off file, found %v", handoffs)
	}
	data, err := os.ReadFile(handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "add rate limiting to the API") {
		t.Errorf("hand-off lost the room's ask:\n%s", data)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--var room_context=") {
		t.Errorf("child started without the room_context var: %s", args)
	}

	// An unknown alias/path is a visible system line, never silence.
	if err := cl.Say("path-room", "/run .loop/pipelines/nope.yaml"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		lines, _ := cl.RoomTranscript("path-room", 0)
		for _, ln := range lines {
			if strings.Contains(ln.Text, "no pipeline") && strings.Contains(ln.Text, "nope.yaml") {
				return true
			}
		}
		return false
	}, "the unknown-path error to land in the transcript")

	// The verb shows in the room's command list, and the workspace
	// pipeline — owned by no alias — is listed as runnable: the
	// composers' pickers and "/run" listings build from this.
	info, ok, err := cl.Room("path-room")
	if err != nil || !ok {
		t.Fatalf("room: %v %v", ok, err)
	}
	found, runnable := false, false
	for _, c := range info.Commands {
		if c.Name == "run" {
			found = true
		}
	}
	for _, p := range info.Runnable {
		if p.Name == ".loop/pipelines/adhoc.yaml" && p.File == ".loop/pipelines/adhoc.yaml" {
			runnable = true
		}
	}
	if !found {
		t.Errorf("/run missing from the room's command list: %+v", info.Commands)
	}
	if !runnable {
		t.Errorf("workspace pipeline not listed as runnable: %+v", info.Runnable)
	}
}

// A room-commanded run must be stoppable by alias — the web sidecar's
// stop button and the terminal /halt ride this exact path: the room
// session's own supervisor interrupts the child, which retires paused.
func TestDaemonRoomHaltStopsRoomRun(t *testing.T) {
	mock := mockAnthropic(t, "unused", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)
	fake := fakeLoop(t, dir)

	cl, _ := startTestServer(t, fake, filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: halt-room\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatal(err)
	}
	// A workspace pipeline the room can run by path; the fake child then
	// parks at its gate, giving the halt something to stop.
	if err := os.MkdirAll(filepath.Join(dir, ".loop/pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loop/pipelines", "demo.yaml"),
		[]byte("name: demo\nstages:\n  - id: only\n    type: tool\n    run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := cl.RoomRun("halt-room", ".loop/pipelines/demo.yaml", "", nil); err != nil {
		t.Fatalf("room run: %v", err)
	}
	// The fake child parks at its gate (waiting on stdin).
	waitFor(t, 10*time.Second, func() bool {
		runs, err := cl.RoomRuns("halt-room")
		if err != nil {
			return false
		}
		for _, r := range runs {
			if r.Alias == "demo" && r.Waiting {
				return true
			}
		}
		return false
	}, "the room run to reach its gate")

	if err := cl.RoomHalt("halt-room", "demo"); err != nil {
		t.Fatalf("room halt: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		runs, _ := cl.RoomRuns("halt-room")
		if len(runs) == 0 {
			return true // retired from the list entirely
		}
		for _, r := range runs {
			if r.Alias == "demo" {
				return !r.Alive
			}
		}
		return false
	}, "the halted room run to retire")

	// The outcome lands in the room's transcript for the agents to see:
	// an interrupted child retires as failed (or paused), never silently.
	waitFor(t, 5*time.Second, func() bool {
		lines, _ := cl.RoomTranscript("halt-room", 0)
		for _, ln := range lines {
			if ln.From == "demo" && (strings.Contains(ln.Text, "paused — resume with") || strings.Contains(ln.Text, "failed — resume with")) {
				return true
			}
		}
		return false
	}, "the halt to show in the room transcript")
}

// The REST run endpoint (the web UI's run button and the attached CLI's
// /run interception ride it) must hand the run the room's context —
// exactly what the typed verb does. It previously skipped the hand-off
// entirely while claiming "same semantics", so the web-side run worked
// purely from the pipeline's baked-in inputs.
func TestDaemonRoomRESTRunCarriesHandoff(t *testing.T) {
	mock := mockAnthropic(t, "unused", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)
	fake := fakeLoop(t, dir)

	cl, _ := startTestServer(t, fake, filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: rest-room\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".loop/pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loop/pipelines", "adhoc.yaml"),
		[]byte("name: adhoc\nstages:\n  - id: only\n    type: tool\n    run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The ask is stated in the room before the REST run starts.
	if err := cl.Say("rest-room", "add rate limiting to the API"); err != nil {
		t.Fatalf("say: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_, err := cl.RoomRun("rest-room", ".loop/pipelines/adhoc.yaml", "", nil)
		return err == nil
	}, "the room to accept the REST run (the ask's turn to finish)")

	// The child carries the hand-off var, and the brief carries the ask.
	waitFor(t, 10*time.Second, func() bool {
		args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
		return err == nil && strings.Contains(string(args), "--var room_context=")
	}, "the child to start with the room_context var")
	handoffs, _ := filepath.Glob(filepath.Join(dir, ".loop", "rooms", "*", "handoff.md"))
	if len(handoffs) != 1 {
		t.Fatalf("expected one hand-off file, found %v", handoffs)
	}
	data, err := os.ReadFile(handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "add rate limiting to the API") {
		t.Errorf("REST-started run's hand-off lost the room's ask:\n%s", data)
	}
}

// An unknown --var key is surfaced, never swallowed: vars merge
// silently downstream, so a misspelled or shell-mangled key would
// otherwise vanish and the run would quietly work from baked-in
// defaults (a mangled "prompt" var was found in a real run's records).
func TestDaemonRoomRunWarnsOnUnknownVar(t *testing.T) {
	mock := mockAnthropic(t, "unused", 0)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)
	fake := fakeLoop(t, dir)

	cl, _ := startTestServer(t, fake, filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: var-room\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".loop/pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loop/pipelines", "idea.yaml"),
		[]byte("name: idea\nvars:\n  idea: \"\"\nstages:\n  - id: only\n    type: tool\n    run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := cl.RoomRun("var-room", ".loop/pipelines/idea.yaml", "", []string{"--var", "prompt=\"implement oauth"}); err != nil {
		t.Fatalf("room run: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		lines, _ := cl.RoomTranscript("var-room", 0)
		for _, ln := range lines {
			if strings.Contains(ln.Text, "declares vars: idea") && strings.Contains(ln.Text, "prompt") {
				return true
			}
		}
		return false
	}, "the unknown-var warning to land in the transcript")
}
