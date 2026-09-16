package webui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/daemon"
)

//go:embed assets templates
var files embed.FS

// pageData is the shell every full page renders inside; fragments use
// the fields they need.
type pageData struct {
	Title string
	// Workspace names the daemon's directory, shown in the nav brand
	// ("loop · webapp") so two project tabs are tellable apart.
	Workspace string
	// FullBleed lets a page own the whole viewport (the room's chat-app
	// frame) instead of the centered document column.
	FullBleed bool
	// page-specific payloads
	Active  []daemon.RunInfo
	History []daemon.RunInfo
	Rooms   []daemon.RoomInfo
	Info    daemon.RunInfo
	Rows    []Row
	Room    daemon.RoomInfo
	Chat    []ChatMsg
	Waiting *daemon.RunInfo
	Sidecar []SidecarRun
	// workspace candidates feeding the dashboard's pickers (nil = the
	// workspace offered none; the forms fall back to typed paths)
	Pipelines []daemon.WorkspaceFile
	WSRooms   []daemon.WorkspaceFile
	// Kind selects which picker frag_workspace renders
	Kind string
}

// SidecarRun is one entry of the room sidecar: the run plus a compact
// timeline rendered from its events.
type SidecarRun struct {
	Info daemon.RunInfo
	Rows []Row
}

// sidecarRuns assembles the expandable per-run details: newest runs
// first, each with the tail of its event timeline.
func sidecarRuns(cl *daemon.Client, name string) []SidecarRun {
	runs, err := cl.RoomRuns(name)
	if err != nil {
		return nil
	}
	if len(runs) > 12 {
		runs = runs[:12]
	}
	out := make([]SidecarRun, 0, len(runs))
	for _, r := range runs {
		rows := []Row{}
		if events, err := cl.Events(r.RunID, 0); err == nil {
			rows = Timeline(events)
			if len(rows) > 15 {
				rows = rows[len(rows)-15:]
			}
		}
		out = append(out, SidecarRun{Info: r, Rows: rows})
	}
	return out
}

// New builds the dashboard: pages and action endpoints rendered from the
// daemon API, /api/* proxied to the daemon's unix socket (SSE included).
func New(version, socket string) (http.Handler, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"phaseLabel": phaseLabel,
		"hms":        func(t time.Time) string { return t.Local().Format("15:04") },
		"hue":        hue,
		"initial":    initial,
		"md":         func(s string) template.HTML { return template.HTML(renderMarkdown(s)) },
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	cl, err := daemon.Dial(socket)
	if err != nil {
		return nil, err
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host, req.Host = "http", "daemon", "daemon"
		},
		Transport: &http.Transport{DialContext: dial},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/", proxy)
	// Assets must revalidate every load: browsers heuristically cache
	// otherwise, and a loop upgrade would serve stale JS/CSS until a
	// hard refresh. Revalidation is a cheap 304 in the steady state.
	mux.Handle("GET /assets/", noCache(http.FileServer(http.FS(files))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { dashboard(tmpl, cl, w) })
	mux.HandleFunc("GET /frag/runs", func(w http.ResponseWriter, r *http.Request) { fragRuns(tmpl, cl, w) })
	mux.HandleFunc("GET /frag/rooms", func(w http.ResponseWriter, r *http.Request) { fragRooms(tmpl, cl, w) })
	mux.HandleFunc("GET /frag/workspace", func(w http.ResponseWriter, r *http.Request) { fragWorkspace(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) { submit(tmpl, cl, w, r) })
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) { runPage(tmpl, cl, w, r) })
	mux.HandleFunc("GET /runs/{id}/timeline", func(w http.ResponseWriter, r *http.Request) { timeline(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/answer", func(w http.ResponseWriter, r *http.Request) { answer(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/halt", func(w http.ResponseWriter, r *http.Request) { halt(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/resume", func(w http.ResponseWriter, r *http.Request) { resume(tmpl, cl, w, r) })
	mux.HandleFunc("GET /rooms/{name}", func(w http.ResponseWriter, r *http.Request) { roomPage(tmpl, cl, w, r) })
	mux.HandleFunc("GET /rooms/{name}/chat", func(w http.ResponseWriter, r *http.Request) { roomChat(tmpl, cl, w, r) })
	mux.HandleFunc("GET /rooms/{name}/sidecar", func(w http.ResponseWriter, r *http.Request) { roomSidecar(tmpl, cl, w, r) })
	mux.HandleFunc("POST /rooms/{name}/say", func(w http.ResponseWriter, r *http.Request) { roomSay(tmpl, cl, w, r) })
	mux.HandleFunc("POST /rooms/{name}/run", func(w http.ResponseWriter, r *http.Request) { roomRun(tmpl, cl, w, r) })
	mux.HandleFunc("POST /rooms/{name}/approve", func(w http.ResponseWriter, r *http.Request) { roomApprove(tmpl, cl, w, r) })
	mux.HandleFunc("POST /rooms/host", func(w http.ResponseWriter, r *http.Request) { roomHost(tmpl, cl, w, r) })
	return mux, nil
}

func renderPage(tmpl *template.Template, w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// noCache forces revalidation of embedded assets so UI updates land on
// the next reload, not "whenever the browser feels like it".
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func dashboard(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	runs, _ := cl.Runs(true)
	workspace := workspaceOf(cl)
	wk, _ := cl.Workspace()
	renderPage(tmpl, w, "dashboard", pageData{
		Title: "runs", Workspace: workspace,
		Active: splitActive(runs), History: splitHistory(runs),
		Pipelines: wk.Pipelines, WSRooms: wk.Rooms,
	})
}

// roomHost opens (or attaches to) a room from the dashboard. A plain
// form (no htmx) so the 303 lands the browser straight on the room.
func roomHost(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.FormValue("file"))
	if file == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	info, err := cl.HostRoom(file)
	if err != nil {
		// Back to the dashboard with the reason in the fragment.
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<p class="error">✗ %s</p>`, template.HTMLEscapeString(err.Error()))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", "/rooms/"+info.Name)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/rooms/"+info.Name, http.StatusSeeOther)
}

func fragRuns(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	runs, _ := cl.Runs(true)
	if err := tmpl.ExecuteTemplate(w, "frag_runs", pageData{Active: splitActive(runs), History: splitHistory(runs)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fragRooms renders the hosted-rooms card. The dashboard preloads it
// once on load and re-fetches only when the user asks — rooms rarely
// change, so they don't ride the runs poll.
func fragRooms(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	rooms, _ := cl.Rooms()
	if err := tmpl.ExecuteTemplate(w, "frag_rooms", pageData{Rooms: rooms}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fragWorkspace re-renders one dashboard picker (kind=pipelines or
// kind=rooms) for the refresh buttons; the pickers are otherwise
// rendered once with the page.
func fragWorkspace(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	wk, _ := cl.Workspace()
	data := pageData{Pipelines: wk.Pipelines, WSRooms: wk.Rooms, Kind: "pipelines"}
	if r.URL.Query().Get("kind") == "rooms" {
		data.Kind = "rooms"
	}
	if err := tmpl.ExecuteTemplate(w, "frag_workspace", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func runPage(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, known, err := cl.Run(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	events, _ := cl.Events(id, 0)
	renderPage(tmpl, w, "run", pageData{Title: id, Workspace: workspaceOf(cl), Info: info, Rows: Timeline(events)})
}

// timeline is the live fragment: htmx refetches it on every streamed
// event, so this one endpoint carries status, gate, and log.
func timeline(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, known, err := cl.Run(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	events, _ := cl.Events(id, 0)
	if err := tmpl.ExecuteTemplate(w, "frag_timeline", pageData{Info: info, Rows: Timeline(events)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func answer(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	text := strings.TrimSpace(r.FormValue("text"))
	if quick := r.FormValue("quick"); quick != "" {
		text = quick
	}
	if text == "" {
		http.Error(w, "empty answer", http.StatusBadRequest)
		return
	}
	if err := cl.Answer(id, text); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// The answer may complete a fast run before the refresh lands.
	if _, known, _ := cl.Run(id); !known {
		fmt.Fprint(w, `<p class="success">✓ answer delivered — the run has finished.</p>`)
		return
	}
	timeline(tmpl, cl, w, r)
}

func halt(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	if err := cl.Halt(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// The halt may retire the run before the refresh lands.
	if info, known, _ := cl.Run(r.PathValue("id")); !known {
		fmt.Fprint(w, `<p class="success">⏸ paused — the run keeps its resume point.</p>`)
		return
	} else if !info.Alive && !info.Waiting {
		fmt.Fprint(w, `<p class="success">⏸ paused — the run keeps its resume point.</p>`)
		return
	}
	timeline(tmpl, cl, w, r)
}

func resume(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, known, err := cl.Run(id)
	if err != nil || !known || info.File == "" {
		http.Error(w, "cannot resume: this server does not know the run's pipeline file (submit it manually with resume)", http.StatusConflict)
		return
	}
	if _, err := cl.Submit(info.File, id, nil); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	timeline(tmpl, cl, w, r)
}

func submit(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	file := strings.TrimSpace(r.FormValue("file"))
	res, err := cl.Submit(file, "", parseVars(r.FormValue("vars")))
	if err != nil {
		fmt.Fprintf(w, `<p class="error">✗ %s</p>`, template.HTMLEscapeString(err.Error()))
		return
	}
	fmt.Fprintf(w, `<p class="success">✓ submitted <a href="/runs/%s">%s</a></p>`, res.RunID, res.RunID)
	if res.Warning != "" {
		fmt.Fprintf(w, `<p class="error">⚠ %s</p>`, template.HTMLEscapeString(res.Warning))
	}
}

// parseVars splits a vars input ("--var a=1 --var b=2" or "a=1 b=2")
// into the API's flat --var arg list.
func parseVars(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		out = append(out, "--var", f)
	}
	return out
}

// ─── rooms ───────────────────────────────────────────────────────────

func roomPage(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	data, ok := roomData(cl, w, r)
	if !ok {
		return
	}
	data.Sidecar = sidecarRuns(cl, data.Room.Name)
	renderPage(tmpl, w, "room", data)
}

// roomSidecar is the expandable pipeline-run panel; it polls on its own
// schedule, independent of the chat stream.
func roomSidecar(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	info, known, err := cl.Room(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "sidecar_fragment", pageData{Room: info, Sidecar: sidecarRuns(cl, info.Name)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// roomData assembles the chat view: messages, the room, and any run
// waiting at a gate. ok=false means the response is already written.
func roomData(cl *daemon.Client, w http.ResponseWriter, r *http.Request) (pageData, bool) {
	info, known, err := cl.Room(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return pageData{}, false
	}
	if !known {
		http.NotFound(w, r)
		return pageData{}, false
	}
	lines, _ := cl.RoomTranscript(info.Name, 0)
	data := pageData{Title: info.Name, Workspace: workspaceOf(cl), FullBleed: true, Room: info, Chat: ChatView(lines, info.Agents, info.Runs)}
	if waiting, ok := anyWaiting(info.Runs); ok {
		data.Waiting = &waiting
	}
	return data, true
}

// roomChat is the room's live body: just the messages and gate card —
// the composer lives outside the swap so typing survives refreshes.
func roomChat(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	data, ok := roomData(cl, w, r)
	if !ok {
		return
	}
	if err := tmpl.ExecuteTemplate(w, "chat_fragment", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func roomSay(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	if err := cl.Say(name, text); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	roomChat(tmpl, cl, w, r)
}

func roomRun(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	alias := strings.TrimSpace(r.FormValue("alias"))
	if alias == "" {
		http.Error(w, "no pipeline chosen", http.StatusBadRequest)
		return
	}
	var resume string
	if v := strings.TrimSpace(r.FormValue("resume")); v != "" {
		resume = v
	}
	if _, err := cl.RoomRun(name, alias, resume, parseVars(r.FormValue("vars"))); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	roomChat(tmpl, cl, w, r)
}

// roomApprove answers a waiting gate of one of the room's runs.
func roomApprove(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	text := strings.TrimSpace(r.FormValue("text"))
	if quick := r.FormValue("quick"); quick != "" {
		text = quick
	}
	if text == "" {
		http.Error(w, "empty answer", http.StatusBadRequest)
		return
	}
	if err := cl.RoomApprove(name, r.FormValue("alias"), text); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	roomChat(tmpl, cl, w, r)
}

// workspaceOf asks the daemon for the workspace name the nav brand
// shows; empty when the daemon can't be reached.
func workspaceOf(cl *daemon.Client) string {
	if p, err := cl.Ping(); err == nil {
		return p.Workspace
	}
	return ""
}

// splitActive/splitHistory divide the daemon's runs list (active first,
// then history) at the first run that is neither running nor waiting.
func splitActive(runs []daemon.RunInfo) []daemon.RunInfo {
	for i, r := range runs {
		if !r.Alive && !r.Waiting {
			return runs[:i]
		}
	}
	return runs
}

func splitHistory(runs []daemon.RunInfo) []daemon.RunInfo {
	for i, r := range runs {
		if !r.Alive && !r.Waiting {
			return runs[i:]
		}
	}
	return nil
}
