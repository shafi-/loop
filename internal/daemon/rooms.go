// Room sessions: chat rooms hosted by the daemon. The session (agents,
// transcript, pipeline supervisor) lives server-side, so a room outlives
// any terminal attached to it and every client — attached CLI, web UI —
// sees the same conversation and can drive the room's pipelines.
//
// The transcript is the stream, same principle as runs and their event
// logs: clients POST messages and watch transcript.jsonl appends. The
// one trade is per-viewer streaming deltas; attach mode delivers
// replies on completion (the direct `loop chat` keeps token streaming).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/runctl"
)

// errRoomBusy is returned when a turn is already in flight: a room
// thinks one thought at a time.
var errRoomBusy = errors.New("the room is still responding — one message at a time")

// roomSession is one hosted chat room.
type roomSession struct {
	cfg      config.Room
	roomPath string // absolute path to the room YAML
	room     *chat.Room
	sup      *runctl.Supervisor
	tr       *chat.Transcript

	mu   sync.Mutex
	busy bool // a turn is being processed
}

// daemonRoomUI absorbs the per-viewer rendering chat.Room emits: in a
// hosted room those are the attached clients' concerns, and the
// transcript already carries the content (replies, tool lines, run
// summaries).
type daemonRoomUI struct{}

func (daemonRoomUI) AgentReplyStart(string)     {}
func (daemonRoomUI) AgentTextDelta(_, _ string) {}
func (daemonRoomUI) AgentsSeen([]string)        {}
func (daemonRoomUI) AgentCapped(string, int)    {}
func (daemonRoomUI) Notice(string, ...any)      {}

// hostRooms owns every hosted room, by room name. A name is a room's
// identity: hosting an already-hosted name attaches to the live session.
type hostRooms struct {
	bin string // the loop binary room-run children are spawned with

	mu    sync.Mutex
	rooms map[string]*roomSession
}

func newHostRooms(bin string) *hostRooms {
	return &hostRooms{bin: bin, rooms: map[string]*roomSession{}}
}

func (h *hostRooms) get(name string) (*roomSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[name]
	return rs, ok
}

func (h *hostRooms) all() []*roomSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*roomSession, 0, len(h.rooms))
	for _, rs := range h.rooms {
		out = append(out, rs)
	}
	return out
}

// host loads (or attaches to) a room session from a room YAML file.
func (h *hostRooms) host(ctx context.Context, file string) (*roomSession, error) {
	cfg, err := config.LoadRoom(file)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	agents, err := buildRoomAgents(cfg)
	if err != nil {
		return nil, err
	}
	tr, err := chat.OpenTranscript(filepath.Join(".loop", "rooms", cfg.Name))
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if rs, ok := h.rooms[cfg.Name]; ok {
		return rs, nil // live room: hosting again attaches
	}
	rs := &roomSession{
		cfg:      *cfg,
		roomPath: abs,
		room:     chat.NewRoom(*cfg, agents, tr),
		tr:       tr,
	}
	rs.sup = runctl.NewSupervisor(h.bin, runctl.Handlers{
		// Run status lands in the room transcript: every attached
		// client watches the room's pipelines through the same stream.
		OnNotice: func(d *runctl.Driver, text string) {
			_ = tr.Append(d.Alias, "▸ "+text)
		},
		OnGate: func(d *runctl.Driver, prompt string) {
			_ = tr.Append(d.Alias, "[approval needed] "+firstLine(prompt)+" — reply yes, no, or your change requests")
		},
		OnEnded: func(d *runctl.Driver, kind, headline string) {
			resume := fmt.Sprintf("resume with /run %s --resume %s", d.Alias, d.RunID)
			switch kind {
			case "completed":
				_ = tr.Append(d.Alias, fmt.Sprintf("completed — run %s; records in .loop/runs/%s/ (state.json = status, events.jsonl = log, context.json = stage outputs)", d.RunID, d.RunID))
			case "paused":
				_ = tr.Append(d.Alias, "paused — "+resume)
			default:
				_ = tr.Append(d.Alias, "failed — "+resume)
			}
		},
	})
	h.rooms[cfg.Name] = rs
	return rs, nil
}

// runIDRe matches loop's generated run ids appearing in transcript text
// (progress lines, completion lines, resume hints).
var runIDRe = regexp.MustCompile(`\b\d{8}-\d{6}-[0-9a-f]{4}\b`)

// mentionedRunIDs returns the distinct run ids this room's transcript
// talks about (progress lines, completion lines), each with the room
// alias that ran it, in order of first mention.
func (rs *roomSession) mentionedRunIDs() []runRef {
	lines, err := readTranscript(rs.transcriptPath(), 0)
	if err != nil {
		return nil
	}
	var out []runRef
	seen := map[string]bool{}
	for _, ln := range lines {
		for _, id := range runIDRe.FindAllString(ln.Text, -1) {
			if !seen[id] {
				seen[id] = true
				out = append(out, runRef{ID: id, Alias: ln.From})
			}
		}
	}
	return out
}

// runRef pairs a mentioned run id with the room alias that ran it.
type runRef struct {
	ID    string
	Alias string
}

// transcriptPath is where this room's transcript.jsonl lives (the
// daemon's working directory is the workspace).
func (rs *roomSession) transcriptPath() string {
	return filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl")
}

// agentNames renders the participants as an @-list for hint lines.
func (rs *roomSession) agentNames() string {
	names := make([]string, 0, len(rs.cfg.Agents))
	for _, a := range rs.cfg.Agents {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", @")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// say accepts one user message and processes the turn in the
// background; the transcript carries everything clients need. One turn
// at a time per room.
func (rs *roomSession) say(ctx context.Context, text string) error {
	rs.mu.Lock()
	if rs.busy {
		rs.mu.Unlock()
		return errRoomBusy
	}
	rs.busy = true
	rs.mu.Unlock()
	go func() {
		defer func() {
			rs.mu.Lock()
			rs.busy = false
			rs.mu.Unlock()
		}()
		// Turn failures never kill the room, but they must be visible:
		// the transcript is the room's record, so failures land there.
		before := countLines(rs.transcriptPath())
		err := rs.room.Say(ctx, text, daemonRoomUI{})
		after := countLines(rs.transcriptPath())
		if err != nil {
			_ = rs.tr.Append("system", "turn failed: "+err.Error())
			return
		}
		// A turn that produced nothing (mistyped @name, everyone chose
		// silence) must not look like the room ignored the user.
		if after-before <= 1 {
			_ = rs.tr.Append("system", "no agent replied — address @"+rs.agentNames()+" to force an answer")
		}
	}()
	return nil
}

func (rs *roomSession) busyNow() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.busy
}

// resetRoom archives the whole conversation and starts a fresh one;
// the transcript file is rotated, nothing is destroyed.
func (rs *roomSession) resetRoom() (string, error) {
	if err := rs.rotateGuard(); err != nil {
		return "", err
	}
	return rs.room.Rotate(0, "reset")
}

// forkRoom keeps the first through transcript lines, inclusive, and
// archives everything after them.
func (rs *roomSession) forkRoom(through int) (string, error) {
	if err := rs.rotateGuard(); err != nil {
		return "", err
	}
	return rs.room.Rotate(through, "fork")
}

// rotateGuard: the conversation can only be rewritten when nothing is
// reading or extending it — no agent turn in flight, no pipeline run
// alive or waiting at a gate.
func (rs *roomSession) rotateGuard() error {
	if rs.busyNow() {
		return errors.New("the room is mid-turn — wait for the reply, then reset or fork")
	}
	for _, r := range rs.sup.Runs() {
		alias := r.Alias
		if alias == "" {
			alias = r.RunID
		}
		if r.Alive || r.Waiting {
			return fmt.Errorf("pipeline %s is still active — halt it before resetting or forking", alias)
		}
	}
	return nil
}

// run starts one of the room's owned pipelines in the room's own
// supervisor — the same aliasing the chat surface uses. Files resolve
// relative to the room file's directory, so the room travels with its
// pipelines.
func (rs *roomSession) run(alias, resumeID string, vars []string) (string, error) {
	file, ok := resolveRoomPipeline(rs.roomPath, &rs.cfg, alias)
	if !ok {
		return "", fmt.Errorf("no pipeline %q in this room", alias)
	}
	return rs.sup.Start(runctl.Spec{Alias: alias, File: file, ResumeID: resumeID, Extra: vars})
}

func resolveRoomPipeline(roomPath string, cfg *config.Room, alias string) (string, bool) {
	for _, p := range cfg.Pipelines {
		if p.Name == alias {
			if filepath.IsAbs(p.File) {
				return p.File, true
			}
			return filepath.Join(filepath.Dir(roomPath), p.File), true
		}
	}
	return "", false
}

// buildRoomAgents resolves a provider per agent — the same rule as the
// chat command: a persona without a model block is env-driven.
func buildRoomAgents(room *config.Room) ([]*agent.Agent, error) {
	factory := engine.DefaultProviderFactory()
	cwd, _ := os.Getwd()
	agents := make([]*agent.Agent, 0, len(room.Agents))
	for i := range room.Agents {
		p := room.Agents[i]
		model := p.Model
		if model == nil {
			model = &config.ModelConfig{}
		}
		provider, err := factory(model)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", p.Name, err)
		}
		agents = append(agents, &agent.Agent{Persona: p, Provider: provider, CWD: cwd})
	}
	return agents, nil
}
