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

	"github.com/shafi-/loop/internal/daemon"
)

//go:embed assets templates
var files embed.FS

// pageData is the shell every full page renders inside; fragments use
// the fields they need.
type pageData struct {
	Title   string
	Version string
	// page-specific payloads
	Active  []daemon.RunInfo
	History []daemon.RunInfo
	Info    daemon.RunInfo
	Rows    []Row
}

// New builds the dashboard: pages and action endpoints rendered from the
// daemon API, /api/* proxied to the daemon's unix socket (SSE included).
func New(version, socket string) (http.Handler, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"phaseLabel": phaseLabel,
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
	mux.Handle("GET /assets/", http.FileServer(http.FS(files)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { dashboard(tmpl, cl, w) })
	mux.HandleFunc("GET /frag/runs", func(w http.ResponseWriter, r *http.Request) { fragRuns(tmpl, cl, w) })
	mux.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) { submit(tmpl, cl, w, r) })
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) { runPage(tmpl, cl, w, r) })
	mux.HandleFunc("GET /runs/{id}/timeline", func(w http.ResponseWriter, r *http.Request) { timeline(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/answer", func(w http.ResponseWriter, r *http.Request) { answer(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/halt", func(w http.ResponseWriter, r *http.Request) { halt(tmpl, cl, w, r) })
	mux.HandleFunc("POST /runs/{id}/resume", func(w http.ResponseWriter, r *http.Request) { resume(tmpl, cl, w, r) })
	return mux, nil
}

func renderPage(tmpl *template.Template, w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func dashboard(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	runs, _ := cl.Runs(true)
	renderPage(tmpl, w, "dashboard", pageData{
		Title: "runs", Version: versionOf(cl), Active: splitActive(runs), History: splitHistory(runs),
	})
}

func fragRuns(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	runs, _ := cl.Runs(true)
	if err := tmpl.ExecuteTemplate(w, "frag_runs", pageData{Active: splitActive(runs), History: splitHistory(runs)}); err != nil {
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
	renderPage(tmpl, w, "run", pageData{Title: id, Version: versionOf(cl), Info: info, Rows: Timeline(events)})
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

func versionOf(cl *daemon.Client) string {
	if p, err := cl.Ping(); err == nil {
		return p.Version
	}
	return "?"
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
