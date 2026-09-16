package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/runctl"
)

// Server is the loop core as a service: it supervises `loop run` children
// and serves the JSON API. Runs are keyed by their run id (the
// supervisor's alias IS the run id here — one naming across the API).
type Server struct {
	sup     *runctl.Supervisor
	rooms   *hostRooms
	version string
	started time.Time
	runsDir string
	// workspace is the base name of the directory the daemon was
	// started from. Children inherit that CWD, so it names the
	// workspace the daemon owns — the web UI's header shows it to
	// tell two project tabs apart.
	workspace string
	// serveCtx lives for the whole Serve call: background work (room
	// turns) runs on it, never on a request context — a 202 must not
	// kill the turn it started.
	serveCtx context.Context

	mu    sync.Mutex
	specs map[string]string // run id -> submitted pipeline file (this daemon's lifetime)
}

// New builds the server. bin is the loop binary children are spawned
// with (production: os.Executable; tests: a fake child).
func New(version, runsDir, bin string) *Server {
	workspace, _ := os.Getwd()
	return &Server{
		sup:       runctl.NewSupervisor(bin, runctl.Handlers{}),
		rooms:     newHostRooms(bin),
		version:   version,
		started:   time.Now(),
		runsDir:   runsDir,
		workspace: filepath.Base(workspace),
		specs:     map[string]string{},
	}
}

// runJSON is the wire form of a supervised run.
type runJSON struct {
	RunID    string `json:"run_id"`
	Alias    string `json:"alias,omitempty"` // the room-side name (room runs)
	Pipeline string `json:"pipeline,omitempty"`
	Phase    string `json:"phase"`
	File     string `json:"file,omitempty"`
	Waiting  bool   `json:"waiting"`
	Prompt   string `json:"prompt,omitempty"`
	Alive    bool   `json:"alive"`
	LastLine string `json:"last_line,omitempty"`
}

func (s *Server) toRunJSON(r runctl.RunInfo) runJSON {
	out := runJSON{
		RunID:    r.RunID,
		Alias:    r.Alias,
		Phase:    "running",
		Waiting:  r.Waiting,
		Prompt:   r.Prompt,
		Alive:    r.Alive,
		LastLine: r.LastLine,
	}
	if r.Waiting {
		out.Phase = "waiting"
	}
	s.mu.Lock()
	out.File = s.specs[r.RunID]
	s.mu.Unlock()
	if name := snapshotName(filepath.Join(s.runsDir, r.RunID, "pipeline.yaml")); name != "" {
		out.Pipeline = name
	}
	return out
}

// snapshotName pulls the pipeline name from a snapshot without paying
// for a full schema parse — the id and phase are the load-bearing data.
func snapshotName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := nameLineRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var nameLineRe = regexp.MustCompile(`(?m)^name:\s*([^\s#]+)`)

// historyRuns lists runs known only from artifacts (finished elsewhere,
// from past daemon lives): id, recorded phase, snapshot name.
func (s *Server) historyRuns() []runJSON {
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		return nil
	}
	var out []runJSON
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if s.supHas(id) {
			continue
		}
		st, err := engine.LoadState(s.runsDir, id)
		if err != nil || st == nil {
			continue
		}
		rj := runJSON{RunID: id, Alive: false}
		switch {
		case st.Done:
			rj.Phase = "done"
		case st.Failed != "":
			rj.Phase = "failed"
		case st.Paused != "":
			rj.Phase = "paused"
		default:
			continue // no recorded stop point: not listable as history
		}
		if name := snapshotName(filepath.Join(s.runsDir, id, "pipeline.yaml")); name != "" {
			rj.Pipeline = name
		}
		out = append(out, rj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID > out[j].RunID }) // ids sort by time
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// queryInt reads a non-negative int query parameter, 0 when absent or
// malformed.
func queryInt(r *http.Request, name string) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// Handler builds the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", s.handlePing)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("POST /api/runs", s.handleSubmit)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("GET /api/runs/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/runs/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /api/runs/{id}/answer", s.handleAnswer)
	mux.HandleFunc("POST /api/runs/{id}/halt", s.handleHalt)
	mux.HandleFunc("POST /api/rooms", s.handleRoomHost)
	mux.HandleFunc("GET /api/rooms", s.handleRooms)
	mux.HandleFunc("GET /api/rooms/{name}", s.handleRoom)
	mux.HandleFunc("GET /api/rooms/{name}/transcript", s.handleRoomTranscript)
	mux.HandleFunc("GET /api/rooms/{name}/runs", s.handleRoomRuns)
	mux.HandleFunc("GET /api/rooms/{name}/stream", s.handleRoomStream)
	mux.HandleFunc("POST /api/rooms/{name}/say", s.handleRoomSay)
	mux.HandleFunc("POST /api/rooms/{name}/run", s.handleRoomRun)
	mux.HandleFunc("POST /api/rooms/{name}/approve", s.handleRoomApprove)
	mux.HandleFunc("POST /api/rooms/{name}/halt", s.handleRoomHalt)
	return mux
}

// Serve accepts connections until ctx is done — or the listener dies —
// then halts every child (resumably) before returning.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.serveCtx = ctx
	errCh := make(chan error, 1)
	go func() { errCh <- http.Serve(ln, s.Handler()) }()

	// Either the caller is done, or the listener broke (closed on us,
	// a failed sidecar bind elsewhere). Either way children shut down
	// resumably — never linger as a daemon that can no longer serve.
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
	}
	s.sup.Shutdown()
	for _, rs := range s.rooms.all() {
		rs.sup.Shutdown()
	}
	if serveErr != nil {
		return serveErr
	}
	return ln.Close()
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"active":         len(s.sup.Runs()),
		"workspace":      s.workspace,
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.sup.Runs()
	out := make([]runJSON, 0, len(runs))
	for _, ru := range runs {
		out = append(out, s.toRunJSON(ru))
	}
	if r.URL.Query().Get("history") == "1" {
		out = append(out, s.historyRuns()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// submitRequest asks the daemon to run a pipeline file. Vars travel as
// ready-made "--var k=v" strings, exactly as the CLI forwards them.
type submitRequest struct {
	File     string   `json:"file"`
	ResumeID string   `json:"resume_id,omitempty"`
	Vars     []string `json:"vars,omitempty"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"file\" path")
		return
	}

	// Concurrent runs are a feature (independent pipelines are fine);
	// overlapping workspaces are the documented risk. The daemon states
	// it, it does not police it.
	warning := ""
	if req.ResumeID == "" {
		if active := s.sup.Runs(); len(active) > 0 {
			names := make([]string, len(active))
			for i, ru := range active {
				names[i] = ru.RunID
			}
			warning = fmt.Sprintf("run(s) %s are also active — concurrent runs share this workspace and can write the same files", strings.Join(names, ", "))
		}
	}

	id := req.ResumeID
	if id == "" {
		id = engine.NewRunID()
	}
	// alias = run id: the API's one name for a run is the id its
	// artifacts live under.
	_, err := s.sup.Start(runctl.Spec{Alias: id, File: req.File, RunID: id, ResumeID: req.ResumeID, Extra: req.Vars})
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.mu.Lock()
	s.specs[id] = req.File
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":        id,
		"events_so_far": countEvents(eventsPath(s.runsDir, id)),
		"warning":       warning,
	})
}

// knownRun reports whether the id is an active run or has run artifacts
// on disk (so historical runs stay queryable).
func (s *Server) knownRun(id string) bool {
	for _, ru := range s.sup.Runs() {
		if ru.RunID == id {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(s.runsDir, id))
	return err == nil
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownRun(id) {
		writeError(w, http.StatusNotFound, "no run %q", id)
		return
	}
	for _, ru := range s.sup.Runs() {
		if ru.RunID == id {
			writeJSON(w, http.StatusOK, s.toRunJSON(ru))
			return
		}
	}
	// Known from artifacts but not active: report its recorded state.
	st, err := engine.LoadState(s.runsDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading state: %v", err)
		return
	}
	out := runJSON{RunID: id, Alive: false, Phase: "running"}
	s.mu.Lock()
	out.File = s.specs[id]
	s.mu.Unlock()
	if st != nil {
		switch {
		case st.Done:
			out.Phase = "done"
		case st.Failed != "":
			out.Phase = "failed"
		case st.Paused != "":
			out.Phase = "paused"
		}
	}
	if name := snapshotName(filepath.Join(s.runsDir, id, "pipeline.yaml")); name != "" {
		out.Pipeline = name
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownRun(id) {
		writeError(w, http.StatusNotFound, "no run %q", id)
		return
	}
	after := 0
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			after = n
		}
	}
	events, err := readEvents(eventsPath(s.runsDir, id), after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading events: %v", err)
		return
	}
	if events == nil {
		events = []EventLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleStream pushes events as they land (SSE). The daemon tails the
// run's events.jsonl — the durable record — so a stream reconnects with
// a plain ?after=N, and the same events serve every client.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.knownRun(id) {
		writeError(w, http.StatusNotFound, "no run %q", id)
		return
	}
	after := 0
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			after = n
		}
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	path := eventsPath(s.runsDir, id)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			events, err := readEvents(path, after)
			if err != nil {
				return
			}
			for _, ev := range events {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"type\":%q,\"event\":%s}\n\n", ev.Seq, ev.Type, ev.Event)
				after = ev.Seq
			}
			if len(events) > 0 {
				fl.Flush()
			}
			// The run ended and the log went quiet: close the stream so
			// clients that missed the terminal event do not hang forever.
			if !s.supHas(id) && len(events) == 0 {
				if st, _ := engine.LoadState(s.runsDir, id); st != nil && (st.Done || st.Failed != "" || st.Paused != "") {
					fmt.Fprint(w, "event: end\ndata: {}\n\n")
					fl.Flush()
					return
				}
			}
		}
	}
}

func (s *Server) supHas(id string) bool {
	for _, ru := range s.sup.Runs() {
		if ru.RunID == id {
			return true
		}
	}
	return false
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"text\" answer")
		return
	}
	err := s.sup.Approve(id, req.Text)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case strings.Contains(err.Error(), "no run named"):
		writeError(w, http.StatusNotFound, "%v", err)
	default:
		writeError(w, http.StatusConflict, "%v", err)
	}
}

func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.sup.Halt(id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case strings.Contains(err.Error(), "nothing to halt"):
		writeError(w, http.StatusNotFound, "no active run %q", id)
	default:
		writeError(w, http.StatusInternalServerError, "%v", err)
	}
}

// ─── rooms ───────────────────────────────────────────────────────────

// roomJSON is the wire form of a hosted room.
type roomJSON struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Agents    []string          `json:"agents"`
	Roles     map[string]string `json:"roles,omitempty"` // agent name -> role, when set
	Pipelines []string          `json:"pipelines"`
	Busy      bool              `json:"busy"`
	Lines     int               `json:"transcript_lines"`
	Runs      []runJSON         `json:"runs"` // the room's own pipeline runs
}

func (s *Server) roomJSON(rs *roomSession) roomJSON {
	out := roomJSON{
		Name:  rs.cfg.Name,
		Path:  rs.roomPath,
		Busy:  rs.busyNow(),
		Roles: map[string]string{},
	}
	for _, a := range rs.cfg.Agents {
		out.Agents = append(out.Agents, a.Name)
		if a.Role != "" {
			out.Roles[a.Name] = a.Role
		}
	}
	for _, p := range rs.cfg.Pipelines {
		out.Pipelines = append(out.Pipelines, p.Name)
	}
	out.Lines = countLines(filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl"))
	for _, ru := range rs.sup.Runs() {
		out.Runs = append(out.Runs, s.toRunJSON(ru))
	}
	return out
}

func (s *Server) handleRoomHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a room \"file\" path")
		return
	}
	rs, err := s.rooms.host(r.Context(), req.File)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.roomJSON(rs))
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	rooms := s.rooms.all()
	out := make([]roomJSON, 0, len(rooms))
	for _, rs := range rooms {
		out = append(out, s.roomJSON(rs))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) roomOf(w http.ResponseWriter, r *http.Request) *roomSession {
	rs, ok := s.rooms.get(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "no hosted room %q", r.PathValue("name"))
		return nil
	}
	return rs
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	if rs := s.roomOf(w, r); rs != nil {
		writeJSON(w, http.StatusOK, s.roomJSON(rs))
	}
}

// handleRoomRuns lists every pipeline run associated with the room —
// active runs first, then runs known from artifacts via transcript
// mentions, newest first. This is the room sidecar's data source.
func (s *Server) handleRoomRuns(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	out := []runJSON{}
	seen := map[string]bool{}
	for _, ru := range rs.sup.Runs() {
		out = append(out, s.toRunJSON(ru))
		seen[ru.RunID] = true
	}
	hist := []runJSON{}
	for _, ref := range rs.mentionedRunIDs() {
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		if rj, ok := s.artifactRun(ref.ID); ok {
			if rj.Alias == "" {
				rj.Alias = ref.Alias
			}
			hist = append(hist, rj)
		}
	}
	sort.Slice(hist, func(i, j int) bool { return hist[i].RunID > hist[j].RunID })
	writeJSON(w, http.StatusOK, map[string]any{"runs": append(out, hist...)})
}

// artifactRun builds a run entry from on-disk artifacts for a run that
// is no longer (or never was) supervised here. Listable only when its
// state recorded a stop point.
func (s *Server) artifactRun(id string) (runJSON, bool) {
	st, err := engine.LoadState(s.runsDir, id)
	if err != nil || st == nil {
		return runJSON{}, false
	}
	out := runJSON{RunID: id, Alive: false}
	switch {
	case st.Done:
		out.Phase = "done"
	case st.Failed != "":
		out.Phase = "failed"
	case st.Paused != "":
		out.Phase = "paused"
	default:
		return runJSON{}, false
	}
	if name := snapshotName(filepath.Join(s.runsDir, id, "pipeline.yaml")); name != "" {
		out.Pipeline = name
	}
	s.mu.Lock()
	out.File = s.specs[id]
	s.mu.Unlock()
	return out, true
}

func (s *Server) handleRoomTranscript(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	after := queryInt(r, "after")
	lines, err := readTranscript(filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl"), after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading transcript: %v", err)
		return
	}
	if lines == nil {
		lines = []TranscriptLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleRoomStream pushes transcript appends (SSE) — the room's live
// feed for every attached client. Rooms never end; the stream closes
// when the client goes away.
func (s *Server) handleRoomStream(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	path := filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl")
	after := queryInt(r, "after")
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			lines, err := readTranscript(path, after)
			if err != nil {
				return
			}
			for _, ln := range lines {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"from\":%q,\"text\":%s}\n\n", ln.Seq, ln.From, mustJSON(ln.Text))
				after = ln.Seq
			}
			if len(lines) > 0 {
				fl.Flush()
			}
		}
	}
}

// mustJSON renders v as a compact JSON value for embedding in an SSE
// data line; text is the only payload and never fails to encode.
func mustJSON(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (s *Server) handleRoomSay(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"text\" message")
		return
	}
	if err := rs.say(s.serveCtx, req.Text); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) handleRoomRun(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias    string   `json:"alias"`
		ResumeID string   `json:"resume_id,omitempty"`
		Vars     []string `json:"vars,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Alias == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a pipeline \"alias\"")
		return
	}
	id, err := rs.run(req.Alias, req.ResumeID, req.Vars)
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.mu.Lock()
	s.specs[id] = req.Alias // room runs keep their alias as the room-side name
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "alias": req.Alias})
}

func (s *Server) handleRoomApprove(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias string `json:"alias,omitempty"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"text\" answer")
		return
	}
	if err := rs.sup.Approve(req.Alias, req.Text); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRoomHalt(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias string `json:"alias,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := rs.sup.Halt(req.Alias); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
