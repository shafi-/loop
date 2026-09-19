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
	"sync"
	"time"

	"github.com/shafi-/loop/internal/generator"
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
	// newGenerator builds a drafting generator on demand: production
	// resolves the environment's provider (the grand rule); tests
	// inject a fake.
	newGenerator func() (*generator.Generator, error)
}

// New builds the server. bin is the loop binary children are spawned
// with (production: os.Executable; tests: a fake child).
func New(version, runsDir, bin string) *Server {
	workspace, _ := os.Getwd()
	s := &Server{
		sup:          runctl.NewSupervisor(bin, runctl.Handlers{}),
		rooms:        newHostRooms(bin),
		version:      version,
		started:      time.Now(),
		runsDir:      runsDir,
		workspace:    filepath.Base(workspace),
		specs:        map[string]string{},
		newGenerator: envGenerator,
	}
	// Deep dives register their generated pipeline file here, so the
	// runs view can resolve (and resume) them like submitted runs.
	s.rooms.registerSpec = func(runID, file string) {
		s.mu.Lock()
		s.specs[runID] = file
		s.mu.Unlock()
	}
	return s
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
	mux.HandleFunc("GET /api/workspace", s.handleWorkspace)
	mux.HandleFunc("POST /api/workspace/draft", s.handleDraft)
	mux.HandleFunc("POST /api/workspace/validate", s.handleValidate)
	mux.HandleFunc("POST /api/workspace/save", s.handleSave)
	mux.HandleFunc("GET /api/personas", s.handlePersonas)
	mux.HandleFunc("GET /api/personas/detail", s.handlePersonaDetail)
	mux.HandleFunc("POST /api/personas/delete", s.handlePersonaDelete)
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
	mux.HandleFunc("POST /api/rooms/{name}/reset", s.handleRoomReset)
	mux.HandleFunc("POST /api/rooms/{name}/fork", s.handleRoomFork)
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
