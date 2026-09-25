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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/roomkit"

	"github.com/shafi-/loop/internal/runctl"
	"github.com/shafi-/loop/internal/usage"
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
	// dives maps deep-dive aliases to their generated pipeline files —
	// run() consults it before alias resolution, so /run dive-<agent>
	// --resume <id> works like any room pipeline.
	dives map[string]string
	// coordinator lands dive reports; nil when the room cannot dive.
	coordinator *roomkit.DiveCoordinator

	mu   sync.Mutex
	busy bool // a turn is being processed
	// pending holds the one shell command a room agent is waiting on
	// (ToolApproval). While set, the turn is parked; the approve
	// endpoint delivers the room's answer. One ask at a time — a room
	// thinks, and asks, one thought at a time.
	pending *toolAsk
}

// toolAsk is one held shell command awaiting the room's answer.
type toolAsk struct {
	agent   string
	command string
	answer  chan string
}

// toolAskTimeout bounds how long a turn waits for the room's answer —
// a question nobody answers must not park the room forever. On timeout
// the command is denied deterministically and the agent carries on.
var toolAskTimeout = 3 * time.Minute

// askToolApproval is the room's ToolApproval: register the ask, show it
// in the room (transcript line → gate card), wait for the answer.
// Strict yes approves; anything else — words, silence, timeout —
// declines, and the words travel back to the agent as the reason.
func (rs *roomSession) askToolApproval(ctx context.Context, agentName, tool, detail string) (bool, string, error) {
	ask := &toolAsk{agent: agentName, command: detail, answer: make(chan string, 1)}
	rs.mu.Lock()
	if rs.pending != nil {
		rs.mu.Unlock()
		return false, "another command approval is already pending", nil
	}
	rs.pending = ask
	rs.mu.Unlock()
	defer func() {
		rs.mu.Lock()
		rs.pending = nil
		rs.mu.Unlock()
	}()

	_ = rs.tr.Append(agentName, fmt.Sprintf("[approval needed] wants to run: %s — answer /approve yes | no", firstLine(detail)))
	select {
	case text := <-ask.answer:
		approved := strings.ToLower(strings.TrimSpace(text)) == "yes" || strings.ToLower(strings.TrimSpace(text)) == "y"
		_ = rs.tr.Append("user", fmt.Sprintf("approve %s: %s", agentName, strings.TrimSpace(text)))
		if approved {
			return true, "", nil
		}
		return false, "the user said: " + strings.TrimSpace(text), nil
	case <-ctx.Done():
		return false, "the turn ended before an answer arrived", nil
	case <-time.After(toolAskTimeout):
		_ = rs.tr.Append("system", fmt.Sprintf("! no answer for @%s's command within %s — denied", agentName, toolAskTimeout))
		return false, "timed out waiting for the room's answer", nil
	}
}

// deliverToolAnswer routes the room's answer to the held command; false
// when no tool ask is pending (the answer belongs to a pipeline gate).
func (rs *roomSession) deliverToolAnswer(text string) bool {
	rs.mu.Lock()
	ask := rs.pending
	rs.mu.Unlock()
	if ask == nil {
		return false
	}
	select {
	case ask.answer <- strings.TrimSpace(text):
	default:
	}
	return true
}

// pendingToolAsk snapshots the held command for the room JSON (the web
// gate card); nil when nothing is asking.
func (rs *roomSession) pendingToolAsk() *ToolAsk {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.pending == nil {
		return nil
	}
	return &ToolAsk{Agent: rs.pending.agent, Command: rs.pending.command}
}

// ToolAsk is one shell command awaiting the room's answer.
type ToolAsk struct {
	Agent   string `json:"agent"`
	Command string `json:"command"`
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

	// registerSpec, when set, records a run id → pipeline file as runs
	// start (deep dives register their generated file; the runs view
	// then resolves and resumes them like submitted runs).
	registerSpec func(runID, file string)

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
	// The session's token ledger rides the room: agent calls record
	// into it, /cost and the API report from it.
	meter := usage.NewMeter()
	tr, err := chat.OpenTranscript(filepath.Join(".loop", "rooms", cfg.Name))
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if rs, ok := h.rooms[cfg.Name]; ok {
		return rs, nil // live room: hosting again attaches
	}
	room, err := roomkit.Room(cfg, tr, meter)
	if err != nil {
		return nil, err
	}
	rs := &roomSession{
		cfg:      *cfg,
		roomPath: abs,
		room:     room,
		tr:       tr,
		dives:    map[string]string{},
	}
	// Deep dives: gated on the executor being ready. The start closure
	// references rs before sup is assigned — safe, because no dive can
	// start until hosting returns and the command paths use rs.sup.
	if coordinator := roomkit.AttachDives(room, tr, tr.Dir(), "",
		func(alias, file string) error {
			id, err := rs.sup.Start(runctl.Spec{Alias: alias, File: file})
			if err == nil && h.registerSpec != nil {
				h.registerSpec(id, file)
			}
			return err
		},
		func(alias, file string) { rs.dives[alias] = file },
		nil, // hosted rooms speak through the transcript, not a terminal
	); coordinator != nil {
		rs.coordinator = coordinator
	}
	// The knowledge layer: agents carry the maintained digest, /notes
	// reads it for zero tokens. Completed pipeline runs refresh it in
	// their own process; session open (below) reconciles it. An
	// unresolvable provider leaves it off — hosting is not held hostage.
	if err := roomkit.AttachKnowledge(room, meter); err != nil {
		_ = tr.Append("system", "knowledge layer off: "+err.Error())
	}
	// Session-open maintenance in the background: first seed or external
	// edits are caught here; fresh costs zero calls, a refresh lands in
	// the transcript where every client sees it. Its spend is flushed
	// to the room's lifetime ledger on the way out.
	go func() {
		rs.room.EnsureKnowledge(context.Background())
		rs.room.FlushUsage()
	}()
	// Typed /reset and /fork meet the same veto as the endpoints: no
	// rotating under a live pipeline run. The verbs live in the room's
	// command registry; the daemon attaches its guard to them by name.
	for _, verb := range []string{"reset", "fork"} {
		if cmd := rs.room.Command(verb); cmd != nil {
			cmd.Guard = func([]string) error { return rs.runsGuard() }
		}
	}
	// Typed /run reaches the room engine too — the web composer and any
	// attached client can start a pipeline without surface-specific
	// commands (the CLI's own /run intercepts earlier; the REST endpoint
	// starts through the same session, so verb and endpoint cannot drift
	// on hand-off semantics).
	// Shell commands from room personas are human-in-the-loop: the ask
	// is held here, shown as a gate card, and answered through the same
	// approve endpoint the pipeline gates use.
	rs.room.ToolApproval = rs.askToolApproval
	if rs.room.Command("run") == nil {
		rs.room.CustomCommands = append(rs.room.CustomCommands, rs.runCommand())
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
			// Dive drivers hand completion to the coordinator first —
			// it lands the report line; the generic summary follows.
			if rs.coordinator != nil && rs.coordinator.Handles(d.Alias) {
				rs.coordinator.OnEnded(d.Alias, d.RunID, kind)
			}
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
		// (The produced-nothing hint is the room engine's call — it
		// knows whether any agent authored a line — not a line-count
		// guess here.)
		if err := rs.room.Say(ctx, text, daemonRoomUI{}); err != nil {
			_ = rs.tr.Append("system", "turn failed: "+err.Error())
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
	return rs.runsGuard()
}

// runsGuard vetoes conversation rotation while a pipeline run is alive
// or waiting. Wired into the room engine itself, so typed /reset and
// /fork hit the same rule as the dedicated endpoints.
func (rs *roomSession) runsGuard() error {
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
// supervisor — the same aliasing the chat surface uses. The name may be
// a room alias OR a workspace-relative/absolute pipeline path: a room
// without a `pipelines:` block can still run any pipeline by file.
// Files resolve relative to the room file's directory for aliases; paths
// resolve relative to the workspace (the daemon's cwd).
func (rs *roomSession) run(alias, resumeID string, vars []string) (string, error) {
	if file, isDive := rs.dives[alias]; isDive {
		return rs.sup.Start(runctl.Spec{Alias: alias, File: file, ResumeID: resumeID, Extra: vars})
	}
	file, label, ok := resolveRoomPipeline(rs.roomPath, &rs.cfg, alias)
	if !ok {
		return "", fmt.Errorf("no pipeline %q in this room — /pipelines lists its aliases, or pass a .loop/pipelines/*.yaml path", alias)
	}
	rs.warnUnknownVars(file, vars)
	return rs.sup.Start(runctl.Spec{Alias: label, File: file, ResumeID: resumeID, Extra: vars})
}

// runWithRoomContext starts a pipeline with the room's hand-off brief:
// WriteHandoff renders it next to the transcript and it travels as the
// room_context var (a caller-provided room_context wins; dives bypass
// this — they embed their context in the generated pipeline). Every
// start path — the typed verb and the REST endpoint — routes through
// here, so the surfaces cannot drift.
func (rs *roomSession) runWithRoomContext(alias, resumeID string, vars []string) (runID, handoff string, err error) {
	if _, isDive := rs.dives[alias]; !isDive {
		if p, werr := rs.room.WriteHandoff(taskVarIn(vars)); werr != nil {
			_ = rs.tr.Append("system", fmt.Sprintf("! context hand-off failed: %v — the run starts without it", werr))
		} else if !hasVar(vars, "room_context") {
			handoff = p
			vars = append(vars, "--var", "room_context="+handoff, "--var", "room="+rs.room.Name)
		}
	}
	runID, err = rs.run(alias, resumeID, vars)
	return runID, handoff, err
}

// taskVarIn extracts the caller's --var task=… value; "" when none.
func taskVarIn(vars []string) string {
	for i := 0; i+1 < len(vars); i++ {
		if vars[i] == "--var" {
			if t, ok := strings.CutPrefix(vars[i+1], "task="); ok {
				return t
			}
		}
	}
	return ""
}

// warnUnknownVars surfaces --var keys the pipeline never declared. Vars
// merge silently downstream, so a misspelled or shell-mangled key would
// otherwise vanish and the run would quietly work from baked-in
// defaults. Parse failure says nothing here — the child reports it.
func (rs *roomSession) warnUnknownVars(file string, vars []string) {
	keys := userVarKeys(vars)
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
	_ = rs.tr.Append("system", fmt.Sprintf("⚠ %s declares vars: %s — %s not among them (check spelling)",
		file, strings.Join(declared, ", "), strings.Join(unknown, ", ")))
}

// userVarKeys returns the keys the caller set with --var k=v, in order.
// The hand-off vars the room itself appends (room_context, room) are
// not the caller's spelling to get wrong, so they never count as user
// keys — warning about them would be a false alarm on every run.
func userVarKeys(vars []string) []string {
	var keys []string
	for i := 0; i+1 < len(vars); i++ {
		if vars[i] == "--var" {
			if k, _, ok := strings.Cut(vars[i+1], "="); ok && k != "" &&
				k != "room_context" && k != "room" {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// resolveRoomPipeline maps a room-owned alias to its pipeline file
// (paths relative to the room file). When no alias matches, the argument
// is accepted as a pipeline path — an existing .yaml/.yml in the
// workspace — and the run's in-room label becomes the file's base name
// without extension.
func resolveRoomPipeline(roomPath string, cfg *config.Room, alias string) (file, label string, ok bool) {
	for _, p := range cfg.Pipelines {
		if p.Name == alias {
			if filepath.IsAbs(p.File) {
				return p.File, alias, true
			}
			return filepath.Join(filepath.Dir(roomPath), p.File), alias, true
		}
	}
	if path, exists := existingPipelinePath(alias); exists {
		return path, pipelineLabel(path), true
	}
	return "", "", false
}

// existingPipelinePath accepts arg as a pipeline file when it names an
// existing .yaml/.yml, relative to the workspace or absolute.
func existingPipelinePath(arg string) (string, bool) {
	if !strings.HasSuffix(arg, ".yaml") && !strings.HasSuffix(arg, ".yml") {
		return "", false
	}
	clean := filepath.Clean(arg)
	if _, err := os.Stat(clean); err != nil {
		return "", false
	}
	return clean, true
}

// pipelineLabel derives a run's in-room name from a path: the base file
// name without extension (".loop/pipelines/review.yaml" → "review").
func pipelineLabel(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
}

// runnablePipelines lists everything /run can start from this room: the
// owned aliases first, then every workspace pipeline file not already
// behind an alias — the same set the composers' pickers and the typed
// "/run" listings show.
func (rs *roomSession) runnablePipelines() []PipelineOption {
	var out []PipelineOption
	seen := map[string]bool{}
	cover := func(file string) string {
		abs, err := filepath.Abs(file)
		if err != nil {
			return file
		}
		return abs
	}
	for _, p := range rs.cfg.Pipelines {
		file := p.File
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(rs.roomPath), file)
		}
		seen[cover(file)] = true
		out = append(out, PipelineOption{Name: p.Name, File: p.File})
	}
	for _, path := range scanPipelineFiles() {
		abs := cover(path)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, PipelineOption{Name: path, File: path})
	}
	return out
}

// scanPipelineFiles returns the workspace's conventionally placed
// pipeline files: .loop/pipelines/*.yaml plus root-level YAML with a
// stages: block (the same classification the dashboard's picker uses).
func scanPipelineFiles() []string {
	var out []string
	pipes := scanYAMLDir(config.PipelinesDir)
	root, _ := classifyRoot(".")
	for _, p := range append(pipes, root...) {
		out = append(out, p.Path)
	}
	sort.Strings(out)
	return out
}

// runCommand seeds the typed /run for hosted rooms: a room-engine verb,
// so every client that types into the composer — the web UI, attached
// CLIs — can start a pipeline without a surface-specific command path.
// With no argument it lists what is runnable here.
func (rs *roomSession) runCommand() chat.CustomCommand {
	return chat.CustomCommand{
		Name: "run",
		Desc: "run a pipeline by room alias or path (/run <name> [--var k=v]…)",
		Handle: func(ctx context.Context, args []string, _ chat.UI) error {
			if len(args) == 0 {
				var names []string
				for _, p := range rs.runnablePipelines() {
					names = append(names, p.Name)
				}
				if len(names) == 0 {
					return fmt.Errorf("nothing to run — add a pipelines: section to the room or create .loop/pipelines/*.yaml")
				}
				return fmt.Errorf("runnable here: %s", strings.Join(names, " · "))
			}
			alias := args[0]
			var resume string
			var vars []string
			for i := 1; i < len(args); i++ {
				switch {
				case args[i] == "--resume" && i+1 < len(args):
					i++
					resume = args[i]
				case args[i] == "--var" && i+1 < len(args):
					i++
					vars = append(vars, "--var", args[i])
				}
			}
			// The hand-off travels inside runWithRoomContext — the same
			// brief the REST run endpoint gets.
			id, handoff, err := rs.runWithRoomContext(alias, resume, vars)
			if err != nil {
				return err
			}
			line := fmt.Sprintf("▶ %s started — run %s; records in .loop/runs/%s/", alias, id, id)
			if handoff != "" {
				line += fmt.Sprintf("; context: %s", handoff)
			}
			return rs.tr.Append("system", line)
		},
	}
}

// hasVar reports whether the var list (the values that follow --var)
// already carries key: user-provided vars win over the ones /run adds.
func hasVar(vars []string, key string) bool {
	for _, v := range vars {
		if strings.HasPrefix(v, key+"=") {
			return true
		}
	}
	return false
}
