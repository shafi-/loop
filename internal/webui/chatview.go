package webui

import (
	"strings"

	"github.com/shafi-/loop/internal/daemon"
)

// ChatMsg is one rendered message in a room's chat view.
type ChatMsg struct {
	Kind string // user | agent | tool | run | gate | system
	From string // display author: persona, run alias, or ""
	Text string
}

// ChatView turns transcript lines into chat-view messages: the user's
// lines as their own bubbles, persona lines as agent bubbles, tool
// activity as compact notices, and run status as centered event lines.
// Classification lives here so templates stay dumb and the shapes are
// unit-testable.
func ChatView(lines []daemon.RoomLine, agents []string) []ChatMsg {
	isAgent := make(map[string]bool, len(agents))
	for _, a := range agents {
		isAgent[a] = true
	}
	out := make([]ChatMsg, 0, len(lines))
	for _, ln := range lines {
		switch {
		case ln.From == "user":
			out = append(out, ChatMsg{Kind: "user", From: "you", Text: ln.Text})
		case ln.From == "system":
			out = append(out, ChatMsg{Kind: "system", Text: ln.Text})
		case isAgent[ln.From]:
			if text, ok := strings.CutPrefix(ln.Text, "[tool] "); ok {
				out = append(out, ChatMsg{Kind: "tool", From: ln.From, Text: text})
			} else {
				out = append(out, ChatMsg{Kind: "agent", From: ln.From, Text: ln.Text})
			}
		default:
			// A run alias speaking: status lines and gate asks.
			text := strings.TrimSpace(ln.Text)
			if _, ok := strings.CutPrefix(text, "▸ "); ok {
				text = text[len("▸ "):]
			}
			kind := "run"
			if strings.Contains(text, "[approval needed]") {
				kind = "gate"
				text = strings.TrimSpace(strings.ReplaceAll(text, "[approval needed]", ""))
			}
			out = append(out, ChatMsg{Kind: kind, From: ln.From, Text: text})
		}
	}
	return out
}

// anyWaiting reports whether one of the room's runs is at a gate — the
// chat view attaches the answer buttons to that fact.
func anyWaiting(runs []daemon.RunInfo) (daemon.RunInfo, bool) {
	for _, r := range runs {
		if r.Waiting {
			return r, true
		}
	}
	return daemon.RunInfo{}, false
}
