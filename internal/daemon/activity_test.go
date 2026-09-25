package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/shafi-/loop/internal/chat"
)

var activityBase = time.Now()

// The SSE emission rule: announce only what persists (sets held for
// activityAfter), clears always immediate, unchanged state never
// resent, and the forced connect snapshot always sends — including the
// empty one, so a pill left over from a dead turn clears on reconnect.
func TestActivityEmission(t *testing.T) {
	thinking := chat.Activity{Agent: "eng", What: "thinking", Since: activityBase}
	fresh := chat.Activity{Agent: "eng", What: "reading app.py", Since: activityBase.Add(time.Second)} // 1s old
	held := chat.Activity{Agent: "eng", What: "reading app.py", Since: activityBase.Add(-3 * time.Second)}
	now := activityBase

	t.Run("fresh activity is held, not announced", func(t *testing.T) {
		key, payload, send := activityEmission("", false, fresh, now, activityAfter)
		if send || key != "" || payload != "" {
			t.Errorf("fresh activity sent: key=%q send=%v payload=%q", key, send, payload)
		}
	})
	t.Run("held activity is announced", func(t *testing.T) {
		key, payload, send := activityEmission("", false, held, now, activityAfter)
		if !send || key == "" || !strings.Contains(payload, `"agent":"eng"`) || !strings.Contains(payload, `"kind":"activity"`) {
			t.Errorf("held activity not announced: key=%q send=%v payload=%q", key, send, payload)
		}
	})
	t.Run("still-forming transition keeps the previous pill", func(t *testing.T) {
		key, _, send := activityEmission("eng\x00running: go test ./...", false, fresh, now, activityAfter)
		if send || key != "eng\x00running: go test ./..." {
			t.Errorf("fresh transition dropped the held pill: key=%q send=%v", key, send)
		}
	})
	t.Run("clear is immediate even under the hold", func(t *testing.T) {
		key, payload, send := activityEmission("eng\x00thinking", false, chat.Activity{}, now, activityAfter)
		if !send || key != "" || !strings.Contains(payload, `"what":""`) {
			t.Errorf("clear not sent: key=%q send=%v payload=%q", key, send, payload)
		}
	})
	t.Run("unchanged state is never resent", func(t *testing.T) {
		_, _, send := activityEmission("eng\x00thinking", false, thinking, now, activityAfter)
		if send {
			t.Error("unchanged activity resent")
		}
	})
	t.Run("connect snapshot always sends, including empty", func(t *testing.T) {
		_, payload, send := activityEmission("", true, chat.Activity{}, now, activityAfter)
		if !send || !strings.Contains(payload, `"kind":"activity"`) {
			t.Errorf("empty connect snapshot not sent: send=%v payload=%q", send, payload)
		}
	})
}

// A1 regression: activity frames riding the room stream are filtered
// before they can surface as blank transcript lines in attached CLIs.
func TestParseRoomLineFiltersActivity(t *testing.T) {
	if ln, ok := parseRoomLine(`data: {"seq":7,"from":"eng","text":"hi"}`); !ok || ln.From != "eng" || ln.Text != "hi" {
		t.Errorf("transcript line mangled: %+v ok=%v", ln, ok)
	}
	if ln, ok := parseRoomLine(`data: {"kind":"activity","agent":"eng","what":"running: x"}`); ok {
		t.Errorf("activity frame leaked as a transcript line: %+v", ln)
	}
	if _, ok := parseRoomLine(`event: activity`); ok {
		t.Error("event line parsed as data")
	}
}
