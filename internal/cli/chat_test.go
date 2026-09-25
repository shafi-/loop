package cli

import (
	"testing"

	"github.com/shafi-/loop/internal/chat"
)

// The terminal watcher announces new activity once, skips per-agent
// "thinking" (the reply header covers it), and never repeats a state.
func TestActivityAnnounce(t *testing.T) {
	last := ""
	last, line, say := activityAnnounce(last, chat.Activity{Agent: "", What: "the room is thinking"})
	if !say || line != "the room is thinking" {
		t.Errorf("room-level thinking not announced: %q %v", line, say)
	}
	if last, line, say = activityAnnounce(last, chat.Activity{Agent: "eng", What: "thinking"}); say {
		t.Errorf("per-agent thinking announced: %q", line)
	}
	last, line, say = activityAnnounce(last, chat.Activity{Agent: "eng", What: "running: go test"})
	if !say || line != "🔧 @eng running: go test" {
		t.Errorf("running not announced: %q %v", line, say)
	}
	if _, line, say = activityAnnounce(last, chat.Activity{Agent: "eng", What: "running: go test"}); say {
		t.Errorf("unchanged state repeated: %q", line)
	}
	if _, line, say = activityAnnounce(last, chat.Activity{}); say {
		t.Errorf("idle announced: %q", line)
	}
}
