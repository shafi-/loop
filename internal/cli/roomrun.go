// Room-run plumbing: a chat room commands real `loop run` subprocesses
// through the shared runctl supervisor. This file is only the room's
// accent on the core: alias→file resolution against the room file, and
// rendering (notices, the approval banner, transcript audit lines).
// Process and gate state live in internal/runctl.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/roomkit"
	"github.com/shafi-/loop/internal/runctl"
)

// roomRunSession adapts the shared supervisor to one room: it resolves
// the room's pipeline aliases and renders supervisor activity as chat
// notices and transcript lines the room's agents can read.
type roomRunSession struct {
	sup        *runctl.Supervisor
	pipelines  []config.RoomPipeline
	roomPath   string
	room       *chat.Room // the conversation whose context runs receive
	ui         chat.UI
	out        io.Writer
	transcript *chat.Transcript
	// dives maps deep-dive aliases to their generated pipeline files —
	// resolveFile consults it, so /approve <alias>, /run <alias>
	// --resume <id>, and /status all work on dives like on pipelines.
	dives map[string]string
	// coordinator lands dive reports; nil when the room cannot dive.
	coordinator *roomkit.DiveCoordinator
}

func newRoomRunSession(bin, roomPath string, pipelines []config.RoomPipeline, room *chat.Room, ui chat.UI, out io.Writer, transcript *chat.Transcript) *roomRunSession {
	s := &roomRunSession{
		pipelines:  pipelines,
		roomPath:   roomPath,
		room:       room,
		ui:         ui,
		out:        out,
		transcript: transcript,
		dives:      map[string]string{},
	}
	s.sup = runctl.NewSupervisor(bin, runctl.Handlers{
		OnNotice: s.onRunNotice,
		OnGate:   s.onRunGate,
		OnEnded:  s.onRunEnded,
	})
	return s
}

// attachDives wires deep-dive launching: availability gate, the room's
// StartDive hook, and handler delegation for dive drivers. Called once
// after the session exists; a nil coordinator = this room cannot dive
// (the reason is on the room for /deepdive to report).
func (s *roomRunSession) attachDives() {
	s.coordinator = roomkit.AttachDives(s.room, s.transcript, s.transcript.Dir(), "",
		func(alias, file string) error {
			_, err := s.sup.Start(runctl.Spec{Alias: alias, File: file})
			return err
		},
		func(alias, file string) { s.dives[alias] = file },
		func(format string, args ...any) { s.ui.Notice(format, args...) },
	)
}

// runsBusy reports whether any supervised run (pipeline or dive) is
// alive or waiting — the rotation veto.
func (s *roomRunSession) runsBusy() error {
	for _, r := range s.sup.Runs() {
		alias := r.Alias
		if alias == "" {
			alias = r.RunID
		}
		if r.Alive || r.Waiting {
			return fmt.Errorf("%s is still active — halt it before resetting or forking", alias)
		}
	}
	return nil
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
// transcript (so the room's agents see the outcome). Dive drivers hand
// their completion to the coordinator first — it lands the report.
func (s *roomRunSession) onRunEnded(d *runctl.Driver, kind, headline string) {
	if s.coordinator != nil && s.coordinator.Handles(d.Alias) {
		s.coordinator.OnEnded(d.Alias, d.RunID, kind)
	}
	resume := fmt.Sprintf("resume with: /run %s --resume %s", d.Alias, d.RunID)
	switch kind {
	case "completed":
		s.ui.Notice("✓ %s completed (run %s) — outputs in .loop/runs/%s/ and stage-written files", d.Alias, d.RunID, d.RunID)
		_ = s.transcript.Append(d.Alias, fmt.Sprintf("completed — run %s; records in .loop/runs/%s/ (state.json = status, events.jsonl = log, context.json = stage outputs)", d.RunID, d.RunID))
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
// room file, so a room and its pipelines travel together. When no alias
// matches, the argument is accepted as a pipeline path — an existing
// .yaml/.yml in the workspace — and the run's label becomes the file's
// base name without extension.
func (s *roomRunSession) resolveFile(alias string) (file, label string, ok bool) {
	if f, isDive := s.dives[alias]; isDive {
		return f, alias, true
	}
	for _, p := range s.pipelines {
		if p.Name == alias {
			if filepath.IsAbs(p.File) {
				return p.File, alias, true
			}
			return filepath.Join(filepath.Dir(s.roomPath), p.File), alias, true
		}
	}
	if !strings.HasSuffix(alias, ".yaml") && !strings.HasSuffix(alias, ".yml") {
		return "", "", false
	}
	clean := filepath.Clean(alias)
	if _, err := os.Stat(clean); err != nil {
		return "", "", false
	}
	base := filepath.Base(clean)
	return clean, strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml"), true
}

// Start launches one run under the room's alias or pipeline path. The
// run receives the room's context: the hand-off brief (task, the asks
// as stated, plan files, digest pointer) travels as the room_context
// var — pipelines that don't read the var ignore it, and a user-provided
// room_context wins.
func (s *roomRunSession) Start(alias, resumeID string, extra []string) error {
	file, label, ok := s.resolveFile(alias)
	if !ok {
		return fmt.Errorf("no pipeline %q in this room — /pipelines lists its aliases, or pass a .loop/pipelines/*.yaml path", alias)
	}
	s.warnUnknownVars(file, extra)
	if s.room != nil {
		if p, err := s.room.WriteHandoff(taskVar(extra)); err == nil && !hasVarArg(extra, "room_context") {
			extra = append(extra, "--var", "room_context="+p, "--var", "room="+s.room.Name)
		}
	}
	_, err := s.sup.Start(runctl.Spec{Alias: label, File: file, ResumeID: resumeID, Extra: extra})
	return err
}

// warnUnknownVars surfaces --var keys the pipeline never declared. Vars
// merge silently downstream, so a misspelled or shell-mangled key would
// otherwise vanish and the run would quietly work from baked-in
// defaults. Parse failure says nothing here — the child reports it.
func (s *roomRunSession) warnUnknownVars(file string, extra []string) {
	keys := userVarKeys(extra)
	if len(keys) == 0 {
		return
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return
	}
	p, err := config.ParsePipeline(src)
	if err != nil {
		return
	}
	var unknown []string
	for _, k := range keys {
		if _, ok := p.Vars[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return
	}
	declared := make([]string, 0, len(p.Vars))
	for k := range p.Vars {
		declared = append(declared, k)
	}
	sort.Strings(declared)
	s.ui.Notice("⚠ %s declares vars: %s — %s not among them (check spelling)",
		file, strings.Join(declared, ", "), strings.Join(unknown, ", "))
}

// userVarKeys returns the keys the caller set with --var k=v, in order.
func userVarKeys(extra []string) []string {
	var keys []string
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--var" {
			if k, _, ok := strings.Cut(extra[i+1], "="); ok && k != "" {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// taskVar extracts the user's --var task=… value from the raw argument
// list; "" when none was given.
func taskVar(extra []string) string {
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--var" {
			if t, ok := strings.CutPrefix(extra[i+1], "task="); ok {
				return t
			}
		}
	}
	return ""
}

// hasVarArg reports whether the raw argument list already sets key.
func hasVarArg(extra []string, key string) bool {
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--var" && strings.HasPrefix(extra[i+1], key+"=") {
			return true
		}
	}
	return false
}

// Runnable lists everything /run accepts by short name here: the room's
// owned aliases, then .loop/pipelines/*.yaml files not already behind an
// alias. The "/run with no args" listing and /pipelines print it.
func (s *roomRunSession) Runnable() []string {
	var names []string
	seen := map[string]bool{}
	cover := func(file string) string {
		abs, err := filepath.Abs(file)
		if err != nil {
			return file
		}
		return abs
	}
	for _, p := range s.pipelines {
		file := p.File
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(s.roomPath), file)
		}
		seen[cover(file)] = true
		names = append(names, p.Name)
	}
	var paths []string
	for _, pat := range []string{config.PipelinesDir + "/*.yaml", config.PipelinesDir + "/*.yml"} {
		matches, _ := filepath.Glob(pat)
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if seen[cover(path)] {
			continue
		}
		seen[cover(path)] = true
		names = append(names, filepath.ToSlash(path))
	}
	return names
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
