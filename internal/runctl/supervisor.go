package runctl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/engine"
)

// Spec is a fully-resolved run request: the host has already mapped its
// own naming (a room alias, a UI form) onto a concrete pipeline file.
type Spec struct {
	Alias    string   // host-side label (room alias, dashboard label)
	File     string   // pipeline YAML, absolute or host-cwd-relative
	ResumeID string   // non-empty: resume this run instead of starting fresh
	Extra    []string // forwarded to the child verbatim (e.g. --var k=v)
}

// Driver is one child `loop run` process. Identity fields are immutable
// after Start; state accessors are safe for concurrent use.
type Driver struct {
	Alias string
	File  string
	RunID string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	waiting  bool // a gate is asking
	lastLine string
	done     chan struct{}
	ended    bool
}

// Waiting reports whether a gate is asking on this run.
func (d *Driver) Waiting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waiting
}

// LastLine is the most recent progress line the child emitted.
func (d *Driver) LastLine() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastLine
}

// Alive reports whether the child is still running.
func (d *Driver) Alive() bool {
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// Halt interrupts the child; its signal wiring records the pause point.
func (d *Driver) Halt() error {
	return d.cmd.Process.Signal(os.Interrupt)
}

// markEnded latches the run's end exactly once.
func (d *Driver) markEnded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ended {
		return false
	}
	d.ended = true
	close(d.done)
	return true
}

// RunInfo is a point-in-time snapshot of one supervised run.
type RunInfo struct {
	Alias    string
	RunID    string
	Waiting  bool   // a gate is asking
	Alive    bool   // child still running
	LastLine string // latest progress line, "" before the first
}

// Handlers receives supervisor activity for the host to render. Callbacks
// run on the supervisor's reader/waiter goroutines; the host owns their
// concurrency safety. All are optional.
type Handlers struct {
	// OnNotice reports a child progress line.
	OnNotice func(d *Driver, text string)
	// OnGate reports an open gate with the full question. The host
	// renders it however its surface answers (/approve, POST /answer).
	OnGate func(d *Driver, prompt string)
	// OnEnded reports the terminal outcome exactly once per run: kind
	// is "completed", "paused", or "failed"; headline is the child's
	// own summary line (or a synthesized one if it died silently).
	OnEnded func(d *Driver, kind, headline string)
}

// Supervisor tracks the active runs of one host: one child per alias,
// many aliases concurrent. Rendering is delegated to Handlers; the
// supervisor itself only maintains process and gate state.
type Supervisor struct {
	bin      string
	handlers Handlers

	mu   sync.Mutex
	runs map[string]*Driver // by alias; removed when the run ends
}

// NewSupervisor builds a supervisor spawning bin (production: the loop
// binary itself; tests: a fake child).
func NewSupervisor(bin string, h Handlers) *Supervisor {
	return &Supervisor{bin: bin, handlers: h, runs: map[string]*Driver{}}
}

// Start launches one run; extra args (e.g. --var k=v) are forwarded to
// the child verbatim. One active run per alias. The busy check and the
// map insert share one lock section (the spawn is a fork/exec, fast)
// so two overlapping Starts cannot both claim an alias.
func (s *Supervisor) Start(spec Spec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.runs[spec.Alias]; busy {
		return fmt.Errorf("%s is already running — /status shows it, or /halt stops it", spec.Alias)
	}

	runID := spec.ResumeID
	args := []string{"run", spec.File}
	if runID != "" {
		args = append(args, "--resume", runID)
	} else {
		runID = engine.NewRunID()
		args = append(args, "--run-id", runID)
	}
	args = append(args, spec.Extra...)

	cmd := exec.Command(s.bin, args...)
	cmd.Stdout = nil // llm streaming stays out of the host: output lives in files
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

	d := &Driver{Alias: spec.Alias, File: spec.File, RunID: runID, cmd: cmd, stdin: stdin, done: make(chan struct{})}
	s.runs[spec.Alias] = d

	tr := &Translator{
		Notice: func(text string) {
			d.mu.Lock()
			d.lastLine = text
			d.waiting = false
			d.mu.Unlock()
			if s.handlers.OnNotice != nil {
				s.handlers.OnNotice(d, text)
			}
		},
		Gate: func(prompt string) {
			d.mu.Lock()
			d.waiting = true
			d.mu.Unlock()
			if s.handlers.OnGate != nil {
				s.handlers.OnGate(d, prompt)
			}
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
			s.finish(d, "failed", fmt.Sprintf("✗ %s exited: %v", d.Alias, err))
		} else {
			s.finish(d, "completed", fmt.Sprintf("✓ %s finished", d.Alias))
		}
	}()
	return nil
}

// finish retires a run exactly once and hands the outcome to the host.
func (s *Supervisor) finish(d *Driver, kind, headline string) {
	if !d.markEnded() {
		return
	}
	s.mu.Lock()
	delete(s.runs, d.Alias)
	s.mu.Unlock()
	if s.handlers.OnEnded != nil {
		s.handlers.OnEnded(d, kind, headline)
	}
}

// Approve answers a waiting gate. With one waiter the bare form (empty
// alias) works; with several, the answer must name the alias.
func (s *Supervisor) Approve(alias, answer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var waiters []*Driver
	for _, d := range s.runs {
		if d.Waiting() {
			waiters = append(waiters, d)
		}
	}
	if len(waiters) == 0 {
		return fmt.Errorf("no pipeline is asking for approval right now")
	}
	var target *Driver
	if alias != "" {
		d, ok := s.runs[alias]
		if !ok {
			return fmt.Errorf("no run named %q", alias)
		}
		if !d.Waiting() {
			return fmt.Errorf("%s is running but not asking for approval", alias)
		}
		target = d
	} else if len(waiters) == 1 {
		target = waiters[0]
	} else {
		names := make([]string, len(waiters))
		for i, d := range waiters {
			names[i] = d.Alias
		}
		return fmt.Errorf("several gates are waiting (%s) — name one: /approve <alias> <answer>", strings.Join(names, ", "))
	}
	if _, err := io.WriteString(target.stdin, answer+"\n"); err != nil {
		return fmt.Errorf("delivering the answer failed — the run may have ended")
	}
	return nil
}

// WaitingGate returns the alias of the run whose gate is asking.
func (s *Supervisor) WaitingGate() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for alias, d := range s.runs {
		if d.Waiting() {
			return alias, true
		}
	}
	return "", false
}

// Halt stops one (alias given) or the single active run.
func (s *Supervisor) Halt(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []*Driver
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
			names[i] = d.Alias
		}
		return fmt.Errorf("several runs are active (%s) — name one: /halt <alias>", strings.Join(names, ", "))
	}
	for _, d := range targets {
		if err := d.Halt(); err != nil {
			return fmt.Errorf("halting %s: %w", d.Alias, err)
		}
	}
	return nil
}

// Runs snapshots every active run for display.
func (s *Supervisor) Runs() []RunInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RunInfo, 0, len(s.runs))
	for _, d := range s.runs {
		out = append(out, RunInfo{
			Alias:    d.Alias,
			RunID:    d.RunID,
			Waiting:  d.Waiting(),
			Alive:    d.Alive(),
			LastLine: d.LastLine(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// Shutdown halts everything (host /quit) and waits briefly; runs halted
// here keep their resume points.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	var ds []*Driver
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
