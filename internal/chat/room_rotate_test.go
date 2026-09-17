package chat

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// The rotation verbs are registered room commands, dispatched in
// Room.Say — the interface every client drives. Whatever the surface
// (local CLI, attached CLI, web composer through the daemon), a verb
// means the same thing; its guard can veto, and feedback is a system
// line in the transcript.
func TestRoomSayRotationCommands(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Append("user", "earlier discussion")
	_ = tr.Append("scout", "earlier reply")
	r := NewRoom(config.Room{Name: "demo"}, nil, tr)
	var vetoes int
	if cmd := r.Command("reset"); cmd == nil {
		t.Fatal("no reset command registered")
	} else {
		cmd.Guard = func(args []string) error {
			vetoes++
			return nil
		}
	}
	ui := &recorderUI{}
	ctx := context.Background()

	// /reset through Say: the conversation is replaced, the note lands
	// as a system line, and the caller hears the confirmation.
	if err := r.Say(ctx, "/reset", ui); err != nil {
		t.Fatalf("/reset: %v", err)
	}
	if len(tr.Messages) != 1 || tr.Messages[0].From == "user" || !strings.Contains(tr.Messages[0].Text, "room reset") {
		t.Fatalf("after /reset = %+v", tr.Messages)
	}
	if len(ui.notices) != 1 || !strings.Contains(ui.notices[0], "archived as") {
		t.Errorf("notices = %v", ui.notices)
	}
	if vetoes != 1 {
		t.Errorf("the guard ran %d times, want 1", vetoes)
	}

	// /fork <n> keeps a prefix; the command text never becomes a message.
	// (Line 1 after a reset is the reset note itself — forks count
	// transcript lines, whatever they are.)
	_ = tr.Append("user", "fresh start")
	_ = tr.Append("scout", "fresh reply")
	if err := r.Say(ctx, "/fork 1", ui); err != nil {
		t.Fatalf("/fork: %v", err)
	}
	if len(tr.Messages) != 2 || tr.Messages[0].From != "system" || tr.Messages[1].From != "system" {
		t.Fatalf("after /fork 1 = %+v", tr.Messages)
	}
	for _, m := range tr.Messages {
		if m.Text == "/fork 1" || m.Text == "/reset" {
			t.Errorf("a rotation command leaked into the transcript as a message")
		}
	}

	// Usage mistakes are system lines, not failed turns.
	if err := r.Say(ctx, "/fork later", ui); err != nil {
		t.Fatalf("/fork later: %v", err)
	}
	if !strings.Contains(tr.Messages[len(tr.Messages)-1].Text, "usage: /fork <n>") {
		t.Errorf("usage line = %+v", tr.Messages[len(tr.Messages)-1])
	}

	// A guard veto (the daemon's active-runs rule) is a system line too,
	// and the command does not run.
	if cmd := r.Command("reset"); cmd == nil {
		t.Fatal("no reset command")
	} else {
		cmd.Guard = func(args []string) error { return errors.New("pipeline x is still active") }
	}
	before := len(tr.Messages)
	if err := r.Say(ctx, "/reset", ui); err != nil {
		t.Fatalf("vetoed /reset: %v", err)
	}
	if len(tr.Messages) != before+1 ||
		!strings.Contains(tr.Messages[len(tr.Messages)-1].Text, "pipeline x is still active") {
		t.Errorf("a vetoed reset must not rotate: %+v", tr.Messages)
	}

	// An unknown slash word is still chat — clients may know verbs this
	// room's engine does not.
	if err := r.Say(ctx, "/restart the effort", ui); err != nil {
		t.Fatalf("/restart: %v", err)
	}
	var userLine bool
	for _, m := range tr.Messages {
		if m.From == "user" && m.Text == "/restart the effort" {
			userLine = true
		}
	}
	if !userLine {
		t.Errorf("unknown verb never reached the room as a message")
	}

	// The archive files exist on disk from the successful rotations.
	entries, _ := os.ReadDir(dir)
	var archives int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "transcript-") {
			archives++
		}
	}
	if archives < 2 {
		t.Errorf("archives on disk = %d, want at least 2", archives)
	}
}
