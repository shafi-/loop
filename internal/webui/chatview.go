package webui

import (
	"html/template"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/daemon"
)

// ChatMsg is one rendered message in a room's chat view.
type ChatMsg struct {
	Kind string // user | agent | tool | run | gate | system
	From string // display author: persona, run alias, or ""
	Text string // plain text (tests, fallbacks)
	// HTML is the bubble body for user/agent messages: markdown rendered
	// to safe HTML (raw input is escaped by the renderer). template.HTML
	// because the renderer, not the template, owns escaping.
	HTML template.HTML
	// At is the message's recorded time; zero for legacy lines.
	At time.Time
	// Cont marks a message continuing the previous one from the same
	// author (avatar and header render once per group).
	Cont bool
	// Count collapses consecutive same-author notices (a flailing
	// agent's repeated tool failures become one bubble, newest text).
	Count int
}

// ChatView turns transcript lines into chat-view messages: the user's
// lines as their own bubbles, persona lines as agent bubbles (markdown
// rendered), tool activity as compact notices, and run status as
// centered event lines. Classification lives here so templates stay dumb
// and the shapes are unit-testable.
func ChatView(lines []daemon.RoomLine, agents []string) []ChatMsg {
	isAgent := make(map[string]bool, len(agents))
	for _, a := range agents {
		isAgent[a] = true
	}
	out := make([]ChatMsg, 0, len(lines))
	for _, ln := range lines {
		switch {
		case ln.From == "user":
			out = append(out, ChatMsg{
				Kind: "user", From: "you", Text: ln.Text, At: ln.At,
				HTML: template.HTML(renderMarkdown(ln.Text)),
				Cont: continues(out, "user", "you"),
			})
		case ln.From == "system":
			out = append(out, ChatMsg{Kind: "system", Text: ln.Text, At: ln.At})
		case isAgent[ln.From]:
			if text, ok := strings.CutPrefix(ln.Text, "[tool] "); ok {
				// Consecutive tool notices collapse into the newest one:
				// a flailing agent must not flood the conversation.
				if n := len(out); n > 0 && out[n-1].Kind == "tool" && out[n-1].From == ln.From {
					out[n-1].Text = text
					out[n-1].Count++
				} else {
					out = append(out, ChatMsg{Kind: "tool", From: ln.From, Text: text, At: ln.At, Count: 1})
				}
			} else {
				out = append(out, ChatMsg{
					Kind: "agent", From: ln.From, Text: ln.Text, At: ln.At,
					HTML: template.HTML(renderMarkdown(ln.Text)),
					Cont: continues(out, "agent", ln.From),
				})
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
			out = append(out, ChatMsg{Kind: kind, From: ln.From, Text: text, At: ln.At})
		}
	}
	return out
}

// continues reports whether msg extends the previous output message:
// same kind and author, with nothing in between (event lines break the
// group — a run status between two agent replies reads as two moments).
func continues(out []ChatMsg, kind, from string) bool {
	if n := len(out); n > 0 {
		return out[n-1].Kind == kind && out[n-1].From == from
	}
	return false
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
