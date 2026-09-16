package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/runctl"
)

// Server is the loop core as a service: it supervises `loop run` children
// and serves the JSON API. Runs are keyed by their run id (the
// supervisor's alias IS the run id here — one naming across the API).
type Server struct {
	sup     *runctl.Supervisor
	version string
	started time.Time
	runsDir string
}

// New builds the server. bin is the loop binary children are spawned
// with (production: os.Executable; tests: a fake child).
func New(version, runsDir, bin string) *Server {
	return &Server{
		sup:     runctl.NewSupervisor(bin, runctl.Handlers{}),
		version: version,
		started: time.Now(),
		runsDir: runsDir,
	}
}

// runJSON is the wire form of a supervised run.
type runJSON struct {
	RunID    string `json:"run_id"`
	Waiting  bool   `json:"waiting"`
	Prompt   string `json:"prompt,omitempty"`
	Alive    bool   `json:"alive"`
	LastLine string `json:"last_line,omitempty"`
}

func toRunJSON(r runctl.RunInfo) runJSON {
	return runJSON{RunID: r.RunID, Waiting: r.Waiting, Prompt: r.Prompt, Alive: r.Alive, LastLine: r.LastLine}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
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
	return mux
}

// Serve accepts connections until ctx is done, then halts every child
// (resumably) before returning.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- http.Serve(ln, s.Handler()) }()
	<-ctx.Done()
	s.sup.Shutdown()
	return ln.Close()
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"active":         len(s.sup.Runs()),
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs := s.sup.Runs()
	out := make([]runJSON, 0, len(runs))
	for _, ru := range runs {
		out = append(out, toRunJSON(ru))
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
			writeJSON(w, http.StatusOK, toRunJSON(ru))
			return
		}
	}
	// Known from artifacts but not active: report its recorded state.
	st, err := engine.LoadState(s.runsDir, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading state: %v", err)
		return
	}
	phase := "running"
	if st != nil {
		switch {
		case st.Done:
			phase = "done"
		case st.Failed != "":
			phase = "failed"
		case st.Paused != "":
			phase = "paused"
		}
	}
	writeJSON(w, http.StatusOK, runJSON{RunID: id, Alive: false, LastLine: "phase: " + phase})
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
