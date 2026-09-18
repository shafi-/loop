// Package webui is loop's web dashboard: a server-rendered UI over the
// daemon's JSON API. It is a pure client — every fact it shows comes
// from the daemon (runs, gates, events), and every action it takes
// (submit, answer, halt, resume) goes through the same API the CLI
// uses. Browser-facing /api/* traffic proxies to the daemon's unix
// socket, which is also where the SSE event streams come from.
package webui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shafi-/loop/internal/daemon"
	"github.com/shafi-/loop/internal/usage"
)

// Row is one rendered timeline entry. Classification happens here in
// Go so templates stay dumb and the shapes are unit-testable.
type Row struct {
	Class string // css class: stage, router, gate, ...
	Head  string // the one-line summary
	// Detail carries supporting text (the question, the answer, an
	// error). LongBlk marks it for a collapsible block instead of an
	// inline span — gate prompts embed whole documents.
	Detail  string
	LongBlk bool
}

// detailLimit is the inline/collapsible threshold for row detail text.
const detailLimit = 140

// Timeline renders a run's event log as display rows, oldest first.
func Timeline(events []daemon.EventLine) []Row {
	rows := []Row{}
	for _, ev := range events {
		var p struct {
			Pipeline     string `json:"pipeline"`
			Stage        string `json:"stage"`
			StageType    string `json:"stage_type"`
			Attempt      int    `json:"attempt"`
			Error        string `json:"error"`
			Next         string `json:"next"`
			Text         string `json:"text"`
			Answer       string `json:"answer"`
			Intent       string `json:"intent"`
			Via          string `json:"via"`
			Kind         string `json:"kind"`
			Tool         string `json:"tool"`
			Detail       string `json:"detail"`
			Steps        int    `json:"steps"`
			MaxTokens    int    `json:"max_tokens"`
			Calls        int    `json:"calls"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
		}
		_ = json.Unmarshal(ev.Event, &p)

		row := Row{}
		switch ev.Type {
		case "run_started":
			row = Row{Class: "started", Head: "▶ pipeline " + p.Pipeline}
		case "stage_started":
			row = Row{Class: "stage", Head: "→ " + p.Stage + " (" + p.StageType + ")"}
		case "stage_skipped":
			row = Row{Class: "stage skip", Head: "= " + p.Stage + " (already complete)"}
		case "stage_retry":
			row = Row{Class: "retry", Head: fmt.Sprintf("↻ %s attempt %d failed", p.Stage, p.Attempt), Detail: p.Error}
		case "stage_failed":
			row = Row{Class: "failure", Head: "✗ " + p.Stage + " failed", Detail: p.Error, LongBlk: len(p.Error) > detailLimit}
		case "router_decision":
			row = Row{Class: "router", Head: fmt.Sprintf("⤷ %s routed to %s", p.Stage, p.Next)}
		case "human_prompt":
			row = Row{Class: "gate", Head: "✋ input needed — " + p.Stage, Detail: p.Text, LongBlk: len(p.Text) > detailLimit}
		case "human_answer":
			row = Row{Class: "answer", Head: "↩ answered", Detail: p.Answer, LongBlk: len(p.Answer) > detailLimit}
		case "human_intent":
			row = Row{Class: "intent intent-" + p.Intent, Head: "understood as: " + p.Intent + " (" + p.Via + ")"}
		case "narration":
			row = Row{Class: "narration", Head: p.Text}
		case "executor_event":
			head := "🔧 " + p.Kind
			if p.Tool != "" {
				head += ": " + p.Tool
			}
			row = Row{Class: "executor", Head: head, Detail: p.Detail, LongBlk: len(p.Detail) > detailLimit}
		case "output_truncated":
			row = Row{Class: "retry", Head: fmt.Sprintf("⚠ output truncated at %d tokens", p.MaxTokens)}
		case "usage":
			row = Row{Class: "started", Head: fmt.Sprintf("◈ tokens: %d calls · in %s · out %s",
				p.Calls, usage.Human(p.InputTokens), usage.Human(p.OutputTokens))}
		case "run_completed":
			row = Row{Class: "terminal ok", Head: fmt.Sprintf("✓ run complete (%d steps)", p.Steps)}
		case "run_paused":
			row = Row{Class: "terminal paused", Head: "⏸ paused at " + p.Stage}
		case "run_failed":
			row = Row{Class: "terminal failed", Head: "✗ failed at " + p.Stage}
		default:
			continue // informational kinds the timeline does not show
		}
		rows = append(rows, row)
	}
	return rows
}

// phaseLabel maps the API phase to a human word for badges.
func phaseLabel(phase string) string {
	switch phase {
	case "running":
		return "running"
	case "waiting":
		return "awaiting you"
	case "done":
		return "done"
	case "failed":
		return "failed"
	case "paused":
		return "paused"
	}
	return phase
}

// varsText joins "--var k=v" args into one editable input value.
func varsText(vars []string) string {
	var out []string
	for i := 0; i < len(vars); i++ {
		if vars[i] == "--var" && i+1 < len(vars) {
			out = append(out, vars[i+1])
			i++
		}
	}
	return strings.Join(out, " ")
}
