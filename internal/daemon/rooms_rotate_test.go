package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reset and fork over the live API: reset archives the whole
// conversation and starts fresh; fork keeps a prefix. Both refuse while
// an agent turn is in flight. The transcript file itself is rotated —
// the archive is never destroyed.
func TestDaemonRoomResetAndFork(t *testing.T) {
	// One slow mock: the delay makes a turn observable (busy window for
	// the refusal check) and costs the test under a second.
	mock := mockAnthropic(t, "a reply worth keeping", 250*time.Millisecond)
	pointProvidersAtMock(t, mock)
	dir := t.TempDir()
	t.Chdir(dir)

	cl, _ := startTestServer(t, fakeLoop(t, dir), filepath.Join(dir, "runs"))

	roomYAML := filepath.Join(dir, "room.yaml")
	if err := os.WriteFile(roomYAML, []byte("name: demo-room\nagents:\n  - name: scout\n    role: scout\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.HostRoom(roomYAML); err != nil {
		t.Fatalf("host: %v", err)
	}

	// Refusal mid-turn: while the slow reply is in flight the room is
	// busy, and a reset is rejected without touching anything.
	if err := cl.Say("demo-room", "@scout hello"); err != nil {
		t.Fatalf("say: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		info, ok, _ := cl.Room("demo-room")
		return ok && info.Busy
	}, "the turn to go busy")
	if _, err := cl.RoomReset("demo-room"); err == nil || !strings.Contains(err.Error(), "mid-turn") {
		t.Fatalf("reset during a turn = %v, want a mid-turn refusal", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		info, ok, _ := cl.Room("demo-room")
		return ok && !info.Busy
	}, "the turn to finish")

	// Fork through line 1: the user's line survives, everything after
	// (the reply, any notices) is archived, and the fork note closes the
	// fresh transcript.
	archive, err := cl.RoomFork("demo-room", 1)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !strings.HasPrefix(archive, "transcript-") {
		t.Errorf("archive name = %q", archive)
	}
	lines, _ := cl.RoomTranscript("demo-room", 0)
	if len(lines) != 2 || lines[0].From != "user" || lines[1].From != "system" {
		t.Fatalf("after fork = %+v", lines)
	}
	if !strings.Contains(lines[1].Text, archive) {
		t.Errorf("fork note %q does not name the archive", lines[1].Text)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loop", "rooms", "demo-room", archive)); err != nil {
		t.Errorf("archive file missing: %v", err)
	}

	// A late poll (after=99) resyncs to the rotated transcript instead
	// of skipping everything forever.
	resynced, _ := cl.RoomTranscript("demo-room", 99)
	if len(resynced) != 2 {
		t.Errorf("resync poll = %d lines, want 2", len(resynced))
	}

	// Reset: fresh single-line transcript, the forked one archived.
	archive2, err := cl.RoomReset("demo-room")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if archive2 == archive {
		t.Errorf("two rotations shared an archive name %q", archive2)
	}
	lines, _ = cl.RoomTranscript("demo-room", 0)
	if len(lines) != 1 || lines[0].From != "system" || !strings.Contains(lines[0].Text, "room reset") {
		t.Fatalf("after reset = %+v", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loop", "rooms", "demo-room", archive2)); err != nil {
		t.Errorf("reset archive missing: %v", err)
	}
}
