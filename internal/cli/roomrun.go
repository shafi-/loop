// Room-run plumbing: a chat room commands real `loop run` subprocesses.
// The child's stderr is the push channel (progress one-liners and gate
// questions arrive as they happen); the child's stdin receives /approve
// answers; durable state lives in the run's own artifacts
// (.loop/runs/<id>/), pulled on demand by /status.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
)

// runStatusTranslator turns the child's stderr lines into room events.
// It is a pure state machine: the same lines in, the same events out,
// which is what makes the shapes below testable.
//
// Line shapes come from run.go and terminalHuman:
//
//	run <id> starting: <name> (N stages)      progress
//	→ <stage> (<type>)                        progress
//	⤷ <router> routed to <target>             progress
//	── your input needed ──────…               gate banner (prompt follows)
//	(/pause · /quit · /exit …)                 gate hint: prompt complete
//	✓ run <id> complete (N steps)             ended: completed
//	⏸ run <id> paused at stage "<id>"         ended: paused
//	✗ run <id> failed at stage "<id>": …      ended: failed
type runStatusTranslator struct {
	gateOpen bool
	gateLive bool // a gate prompt was delivered, not yet resolved
	prompt   []string

	Notice func(text string)        // progress one-liner
	Gate   func(prompt string)      // full question; /approve answers it
	Ended  func(kind, headline string)
}

// Feed processes one stderr line (newline-trimmed by the reader).
func (t *runStatusTranslator) Feed(line string) {
	line = strings.TrimRight(line, "\r")
	if t.gateOpen {
		// The hint marker ends the question; the stray "> " that follows
		// never arrives as a line of its own.
		if strings.HasPrefix(line, "(/pause") {
			t.gateOpen = false
			t.gateLive = true
			if t.Gate != nil {
				t.Gate(strings.Join(t.prompt, "\n"))
			}
			t.prompt = nil
			return
		}
		t.prompt = append(t.prompt, line)
		return
	}
	if strings.TrimSpace(line) == "" || strings.TrimSpace(line) == ">" {
		return // filler around prompts and banners
	}
	// The child's terminal prompt ends with a dangling "> " that glues
	// to the next stderr line; strip the marker (prompt content itself
	// is only accumulated inside gate mode, untouched by this).
	line = strings.TrimPrefix(line, "> ")
	switch {
	case strings.HasPrefix(line, "── your input needed"):
		t.gateOpen = true
		t.prompt = nil
		return
	case strings.HasPrefix(line, "✓ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("completed", line)
		}
		return
	case strings.HasPrefix(line, "⏸ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("paused", line)
		}
		return
	case strings.HasPrefix(line, "✗ run "):
		t.resolve()
		if t.Ended != nil {
			t.Ended("failed", line)
		}
		return
	case strings.HasPrefix(line, "  resume with:"):
		return // the child's shell-form hint; the room prints its own
	}
	if t.gateLive {
		t.gateLive = false // work resumed after the answer
	}
	if t.Notice != nil {
		t.Notice(line)
	}
}

// Waiting reports whether a gate question is outstanding.
func (t *runStatusTranslator) Waiting() bool { return t.gateLive }

func (t *runStatusTranslator) resolve() { t.gateLive = false }

// runDriver is one child `loop run` process.
type runDriver struct {
	alias string
	file  string
	runID string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	waiting  bool // a gate is asking
	lastLine string
	done     chan struct{}
	exited   error
	ended    bool
}

// roomRunSession tracks the active runs of one room.
type roomRunSession struct {
	bin        string
	roomPath   string
	pipelines  []config.RoomPipeline
	ui         chat.UI
	out        io.Writer
	transcript *chat.Transcript

	mu   sync.Mutex
	runs map[string]*runDriver // by alias; removed when the run ends
}

func newRoomRunSession(bin, roomPath string, pipelines []config.RoomPipeline, ui chat.UI, out io.Writer, transcript *chat.Transcript) *roomRunSession {
	return &roomRunSession{
		bin:        bin,
		roomPath:   roomPath,
		pipelines:  pipelines,
		ui:         ui,
		out:        out,
		transcript: transcript,
		runs:       map[string]*runDriver{},
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

// Start launches one run; extra args (e.g. --var k=v) are forwarded to
// the child verbatim. One active run per alias.
func (s *roomRunSession) Start(alias string, resumeID string, extra []string) error {
	file, ok := s.resolveFile(alias)
	if !ok {
		return fmt.Errorf("no pipeline %q in this room — /pipelines lists what it owns", alias)
	}
	s.mu.Lock()
	if _, busy := s.runs[alias]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%s is already running — /status shows it, or /halt stops it", alias)
	}
	s.mu.Unlock()

	runID := resumeID
	args := []string{"run", file}
	if runID != "" {
		args = append(args, "--resume", runID)
	} else {
		runID = engine.NewRunID()
		args = append(args, "--run-id", runID)
	}
	args = append(args, extra...)

	cmd := exec.Command(s.bin, args...)
	cmd.Stdout = nil // llm streaming stays out of the room: output lives in files
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", s.bin, err)
	}

	d := &runDriver{alias: alias, file: file, runID: runID, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	s.mu.Lock()
	s.runs[alias] = d
	s.mu.Unlock()

	tr := &runStatusTranslator{
		Notice: func(text string) {
			d.mu.Lock()
			d.lastLine = text
			d.waiting = false
			d.mu.Unlock()
			s.ui.Notice("▸ %s %s", alias, text)
		},
		Gate: func(prompt string) {
			d.mu.Lock()
			d.waiting = true
			d.mu.Unlock()
			fmt.Fprintf(s.out, "\n── %s needs your approval ──────────────────────\n%s\n", alias, prompt)
			fmt.Fprintf(s.out, "answer with: /approve yes · /approve no · /approve <your words>\n")
			_ = s.transcript.Append(alias, "[approval needed] reply /approve yes | no | your change requests")
		},
		Ended: func(kind, headline string) {
			s.finish(d, kind, headline)
		},
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // gate prompts embed whole documents
		for sc.Scan() {
			tr.Feed(sc.Text())
		}
	}()
	go func() {
		err := cmd.Wait()
		// A child that died without a terminal status line still gets a
		// verdict — crashed runs are resumable, silence would hide that.
		if !d.markEnded() {
			return
		}
		if err != nil {
			s.finish(d, "failed", fmt.Sprintf("✗ %s exited: %v", alias, err))
		} else {
			s.finish(d, "completed", fmt.Sprintf("✓ %s finished", alias))
		}
	}()
	return nil
}

// finish reports a run's end exactly once, records the summary in the
// transcript (so the room's agents see the outcome), and retires the
// driver.
func (s *roomRunSession) finish(d *runDriver, kind, headline string) {
	if !d.markEnded() {
		return
	}
	resume := fmt.Sprintf("resume with: /run %s --resume %s", d.alias, d.runID)
	switch kind {
	case "completed":
		s.ui.Notice("✓ %s completed (run %s) — outputs in .loop/runs/%s/ and stage-written files", d.alias, d.runID, d.runID)
		_ = s.transcript.Append(d.alias, fmt.Sprintf("completed — run %s; outputs in .loop/runs/%s/", d.runID, d.runID))
	case "paused":
		s.ui.Notice("%s", headline)
		s.ui.Notice("%s", resume)
		_ = s.transcript.Append(d.alias, fmt.Sprintf("paused — run %s; %s", d.runID, resume))
	default:
		s.ui.Notice("%s", headline)
		s.ui.Notice("%s", resume)
		_ = s.transcript.Append(d.alias, fmt.Sprintf("failed — run %s; %s", d.runID, resume))
	}
	s.mu.Lock()
	delete(s.runs, d.alias)
	s.mu.Unlock()
}

// Approve routes an answer to the waiting gate, if this driver has one.
func (d *runDriver) Approve(answer string) bool {
	if !d.waitingLocked() {
		return false
	}
	if _, err := io.WriteString(d.stdin, answer+"\n"); err != nil {
		return false
	}
	return true
}

// Halt interrupts the child; its signal wiring records the pause point.
func (d *runDriver) Halt() error {
	return d.cmd.Process.Signal(os.Interrupt)
}

// Alive reports whether the child is still running.
func (d *runDriver) Alive() bool {
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// markEnded latches the run's end exactly once.
func (d *runDriver) markEnded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended {
		return false
	}
	d.ended = true
	close(d.done)
	return true
}

// WaitingGate returns the alias of the run whose gate is asking.
func (s *roomRunSession) WaitingGate() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for alias, d := range s.runs {
		if d.waitingLocked() {
			return alias, true
		}
	}
	return "", false
}

// Approve answers a waiting gate. With one waiter the bare form works;
// with several, the answer must name the alias.
func (s *roomRunSession) Approve(alias, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var waiters []*runDriver
	for _, d := range s.runs {
		if d.waitingLocked() {
			waiters = append(waiters, d)
		}
	}
	if len(waiters) == 0 {
		return fmt.Errorf("no pipeline is asking for approval right now")
	}
	var target *runDriver
	if alias != "" {
		d, ok := s.runs[alias]
		if !ok {
			return fmt.Errorf("no run named %q", alias)
		}
		if !d.waitingLocked() {
			return fmt.Errorf("%s is running but not asking for approval", alias)
		}
		target = d
	} else if len(waiters) == 1 {
		target = waiters[0]
	} else {
		names := make([]string, len(waiters))
		for i, d := range waiters {
			names[i] = d.alias
		}
		return fmt.Errorf("several gates are waiting (%s) — name one: /approve <alias> <answer>", strings.Join(names, ", "))
	}
	if _, err := io.WriteString(target.stdin, answer+"\n"); err != nil {
		return fmt.Errorf("delivering the answer failed — the run may have ended")
	}
	return nil
}

// waitingLocked reports gate state under the session lock (the driver
// lock is fine to take here: it is never held while taking the session
// lock, so no cycle).
func (d *runDriver) waitingLocked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waiting
}

// Halt stops one (alias given) or the single active run.
func (s *roomRunSession) Halt(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []*runDriver
	for a, d := range s.runs {
		if alias == "" || a == alias {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("nothing to halt")
	}
	if alias == "" && len(targets) > 1 {
		names := make([]string, len(targets))
		for i, d := range targets {
			names[i] = d.alias
		}
		return fmt.Errorf("several runs are active (%s) — name one: /halt <alias>", strings.Join(names, ", "))
	}
	for _, d := range targets {
		if err := d.Halt(); err != nil {
			return fmt.Errorf("halting %s: %w", d.alias, err)
		}
	}
	return nil
}

// Status renders every active run into lines for the room.
func (s *roomRunSession) Status() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) == 0 {
		return []string{"no active runs — /run <pipeline> starts one"}
	}
	var out []string
	for alias, d := range s.runs {
		d.mu.Lock()
		state, last := "running", d.lastLine
		if d.waiting {
			state = "awaiting approval (/approve …)"
		}
		d.mu.Unlock()
		if !d.Alive() {
			state = "finishing"
		}
		out = append(out, fmt.Sprintf("  %s — run %s [%s] %s", alias, d.runID, state, last))
	}
	sort.Strings(out)
	return append(out, "full logs: .loop/runs/<id>/events.jsonl")
}

// Shutdown halts everything (room /quit) and waits briefly; runs halted
// here keep their resume points.
func (s *roomRunSession) Shutdown() {
	s.mu.Lock()
	var ds []*runDriver
	for _, d := range s.runs {
		ds = append(ds, d)
	}
	s.mu.Unlock()
	for _, d := range ds {
		_ = d.Halt()
	}
	for _, d := range ds {
		select {
		case <-d.done:
		case <-time.After(3 * time.Second):
			_ = d.cmd.Process.Kill()
		}
	}
}
