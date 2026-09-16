package webui

import (
	"fmt"
	"html/template"
	"regexp"
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
	// Live marks the current-status pill of a run still in flight; the
	// template gives it a pulsing dot.
	Live bool
	// Start marks the run's start line: the text is the rephrased
	// sentence ("You started respond pipeline"), so the template skips
	// the pipeline chip the sentence already names.
	Start bool
	// Count collapses consecutive same-author notices (a flailing
	// agent's repeated tool failures become one bubble, newest text).
	Count int
	// Line is the message's last transcript line number — what a "fork
	// here" keeps through. Zero for event lines.
	Line int
	// Fork marks the one message of a user/agent group that carries the
	// fork affordance (the group's tail, so forking keeps the whole
	// message).
	Fork bool
}

// runStartRe matches the child's machine banner ("run <id> starting:
// <pipeline> (N stages)") — the view rephrases it as a sentence.
var runStartRe = regexp.MustCompile(`^run \S+ starting: (.+?)(?: \(\d+ stages?\))?$`)

// ChatView turns transcript lines into chat-view messages: the user's
// lines as their own bubbles, persona lines as agent bubbles (markdown
// rendered), tool activity as compact notices, and run status as
// centered event lines. Classification lives here so templates stay dumb
// and the shapes are unit-testable.
func ChatView(lines []daemon.RoomLine, agents []string, runs []daemon.RunInfo) []ChatMsg {
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
				Line: ln.Seq,
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
					Line: ln.Seq,
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
	return markForkPoints(collapseRuns(out, runs))
}

// markForkPoints hangs the fork affordance on the last message of each
// user/agent group, carrying the group's last transcript line — "fork
// here" keeps the whole message, not just its first bubble.
func markForkPoints(out []ChatMsg) []ChatMsg {
	for i := range out {
		m := &out[i]
		if m.Kind != "user" && m.Kind != "agent" {
			continue
		}
		j := i
		for j+1 < len(out) && out[j+1].Cont && out[j+1].Kind == m.Kind && out[j+1].From == m.From {
			j++
		}
		out[j].Fork = true
	}
	return out
}

// collapseRuns keeps, per run, two lines: the start and the latest one
// (the current or final status) — the room chat carries the bookends,
// the sidecar timeline carries the rest. A "starting:" line opens a run
// and everything after it belongs to that run until the next start, so
// repeated runs of one pipeline each keep their own pair. Gate asks are
// their own kind and always stay. The newest line of a run still in
// flight is marked Live so the pill reads as a live status.
func collapseRuns(in []ChatMsg, runs []daemon.RunInfo) []ChatMsg {
	active := map[string]bool{}
	for _, r := range runs {
		key := r.Alias
		if key == "" {
			key = r.Pipeline
		}
		if r.Alive || r.Waiting {
			active[key] = true
		}
	}
	// Group the lines into runs: per alias, each start bumps the epoch.
	epoch := map[string]int{}
	group := map[int]string{} // message index -> "alias|epoch"
	lastGroup := map[string]string{}
	first, last := map[string]int{}, map[string]int{}
	drop := map[int]bool{}
	for i := range in {
		if in[i].Kind != "run" {
			continue
		}
		alias := in[i].From
		if strings.Contains(in[i].Text, " starting:") {
			epoch[alias]++
		}
		gk := fmt.Sprintf("%s|%d", alias, epoch[alias])
		group[i] = gk
		lastGroup[alias] = gk
		if _, seen := first[gk]; !seen {
			first[gk] = i
			last[gk] = i
			continue
		}
		if last[gk] != first[gk] {
			drop[last[gk]] = true // a middle progress line
		}
		last[gk] = i
	}
	out := make([]ChatMsg, 0, len(in))
	for i := range in {
		if drop[i] {
			continue
		}
		m := in[i]
		if m.Kind == "run" && active[m.From] && lastGroup[m.From] == group[i] && last[group[i]] == i {
			m.Live = true
		}
		out = append(out, m)
	}
	rephraseRunStarts(out)
	return out
}

// rephraseRunStarts turns each run's machine banner into the room's
// sentence: "run <id> starting: respond (5 stages)" reads as "You
// started respond pipeline" — runs are only ever started by the human,
// and the sentence names the pipeline, so the alias chip goes too.
// Names that already say "pipeline" don't get it doubled.
func rephraseRunStarts(msgs []ChatMsg) {
	for i := range msgs {
		m := &msgs[i]
		if m.Kind != "run" {
			continue
		}
		if parts := runStartRe.FindStringSubmatch(m.Text); parts != nil {
			m.Text = "You started " + parts[1]
			if !strings.Contains(strings.ToLower(parts[1]), "pipeline") {
				m.Text += " pipeline"
			}
			m.Start = true
		}
	}
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
