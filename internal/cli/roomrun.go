// Room-run plumbing: a chat room commands real `loop run` subprocesses
// through the shared runctl supervisor. This file is only the room's
// accent on the core: alias→file resolution against the room file, and
// rendering (notices, the approval banner, transcript audit lines).
// Process and gate state live in internal/runctl.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/runctl"
)

// roomRunSession adapts the shared supervisor to one room: it resolves
// the room's pipeline aliases and renders supervisor activity as chat
// notices and transcript lines the room's agents can read.
type roomRunSession struct {
	sup        *runctl.Supervisor
	pipelines  []config.RoomPipeline
	roomPath   string
	ui         chat.UI
	out        io.Writer
	transcript *chat.Transcript
}

func newRoomRunSession(bin, roomPath string, pipelines []config.RoomPipeline, ui chat.UI, out io.Writer, transcript *chat.Transcript) *roomRunSession {
	s := &roomRunSession{
		pipelines:  pipelines,
		roomPath:   roomPath,
		ui:         ui,
		out:        out,
		transcript: transcript,
	}
	s.sup = runctl.NewSupervisor(bin, runctl.Handlers{
		OnNotice: s.onRunNotice,
		OnGate:   s.onRunGate,
		OnEnded:  s.onRunEnded,
	})
	return s
}

func (s *roomRunSession) onRunNotice(d *runctl.Driver, text string) {
	s.ui.Notice("▸ %s %s", d.Alias, text)
}

// onRunGate renders the question the room must answer: a banner in the
// conversation and a compact transcript marker (the room's audit trail,
// and context for the agents).
func (s *roomRunSession) onRunGate(d *runctl.Driver, prompt string) {
	fmt.Fprintf(s.out, "\n── %s needs your approval ──────────────────────\n%s\n", d.Alias, prompt)
	fmt.Fprintf(s.out, "answer with: /approve yes · /approve no · /approve <your words>\n")
	_ = s.transcript.Append(d.Alias, "[approval needed] reply /approve yes | no | your change requests")
}

// onRunEnded reports a run's end and records the summary in the
// transcript (so the room's agents see the outcome).
func (s *roomRunSession) onRunEnded(d *runctl.Driver, kind, headline string) {
	resume := fmt.Sprintf("resume with: /run %s --resume %s", d.Alias, d.RunID)
	switch kind {
	case "completed":
		s.ui.Notice("✓ %s completed (run %s) — outputs in .loop/runs/%s/ and stage-written files", d.Alias, d.RunID, d.RunID)
		_ = s.transcript.Append(d.Alias, fmt.Sprintf("completed — run %s; outputs in .loop/runs/%s/", d.RunID, d.RunID))
	case "paused":
		s.ui.Notice("%s", headline)
		s.ui.Notice("%s", resume)
		_ = s.transcript.Append(d.Alias, fmt.Sprintf("paused — run %s; %s", d.RunID, resume))
	default:
		s.ui.Notice("%s", headline)
		s.ui.Notice("%s", resume)
		_ = s.transcript.Append(d.Alias, fmt.Sprintf("failed — run %s; %s", d.RunID, resume))
	}
}

// resolveFile maps an in-room alias to its pipeline file, relative to the
// room file, so a room and its pipelines travel together.
func (s *roomRunSession) resolveFile(alias string) (string, bool) {
	for _, p := range s.pipelines {
		if p.Name == alias {
			if filepath.IsAbs(p.File) {
				return p.File, true
			}
			return filepath.Join(filepath.Dir(s.roomPath), p.File), true
		}
	}
	return "", false
}

// Start launches one run under the room's alias.
func (s *roomRunSession) Start(alias, resumeID string, extra []string) error {
	file, ok := s.resolveFile(alias)
	if !ok {
		return fmt.Errorf("no pipeline %q in this room — /pipelines lists what it owns", alias)
	}
	return s.sup.Start(runctl.Spec{Alias: alias, File: file, ResumeID: resumeID, Extra: extra})
}

// Approve answers a waiting gate (see runctl.Supervisor.Approve).
func (s *roomRunSession) Approve(alias, answer string) error {
	return s.sup.Approve(alias, answer)
}

// Halt stops one (alias given) or the single active run.
func (s *roomRunSession) Halt(alias string) error {
	return s.sup.Halt(alias)
}

// Shutdown halts everything (room /quit); runs halted here keep their
// resume points.
func (s *roomRunSession) Shutdown() {
	s.sup.Shutdown()
}

// Status renders every active run into lines for the room.
func (s *roomRunSession) Status() []string {
	runs := s.sup.Runs()
	if len(runs) == 0 {
		return []string{"no active runs — /run <pipeline> starts one"}
	}
	var out []string
	for _, r := range runs {
		state := "running"
		if r.Waiting {
			state = "awaiting approval (/approve …)"
		}
		if !r.Alive {
			state = "finishing"
		}
		out = append(out, fmt.Sprintf("  %s — run %s [%s] %s", r.Alias, r.RunID, state, r.LastLine))
	}
	sort.Strings(out)
	return append(out, "full logs: .loop/runs/<id>/events.jsonl")
}
