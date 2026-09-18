package webui

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/daemon"
)

// Creating documents: the describe box drafts with the daemon's
// generator; the YAML editor is the source of truth from then on; and
// the daemon's own strict parser — the same one `loop validate` runs —
// gates every validate and save.

func newPage(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	switch kind {
	case "pipeline", "room", "persona":
	default:
		http.NotFound(w, r)
		return
	}
	data := pageData{Title: "new " + kind, Workspace: workspaceOf(cl), Kind: kind}

	switch kind {
	case "persona":
		form := &personaForm{Scope: "project", Tools: map[string]bool{}}
		// Edit mode: ?scope=&name= prefills the form and the editor
		// from the saved persona.
		if name := r.URL.Query().Get("name"); name != "" {
			scope := r.URL.Query().Get("scope")
			yamlText, err := cl.PersonaDetail(scope, name)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if p, err := config.ParsePersona([]byte(yamlText)); err == nil {
				form.Name, form.Role, form.System = p.Name, p.Role, p.System
				if p.Model != nil {
					form.Provider, form.Model = string(p.Model.Provider), p.Model.Model
				}
				for _, tool := range p.Tools {
					form.Tools[tool] = true
				}
			}
			form.Editing, form.Scope = true, scope
			data.PersonaYAML = yamlText
			data.Title = "edit persona"
		}
		data.Form = form
	case "room":
		// The draft can reference library personas; surface what exists.
		data.Personas, _ = cl.Personas()
	}
	renderPage(tmpl, w, "new", data)
}

// composePersona turns the persona form's fields into YAML, emitted
// structurally so the output always round-trips the strict parser. The
// editor remains the source of truth after composition.
func composePersona(cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	p := config.Persona{
		Name:   strings.TrimSpace(r.FormValue("name")),
		Role:   strings.TrimSpace(r.FormValue("role")),
		System: strings.Trim(r.FormValue("system"), "\n"),
	}
	for _, tool := range r.Form["tools"] {
		p.Tools = append(p.Tools, tool)
	}
	if provider, model := strings.TrimSpace(r.FormValue("provider")), strings.TrimSpace(r.FormValue("model")); provider != "" || model != "" {
		p.Model = &config.ModelConfig{Provider: config.Provider(provider), Model: model}
	}
	out, err := yaml.Marshal(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, template.HTMLEscapeString(string(out)))
}

// draftDoc runs the description past the daemon's generator. The draft
// lands in the editor as raw (escaped) text — the swap target is the
// textarea itself, so its content simply becomes the draft.
func draftDoc(cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	desc := strings.TrimSpace(r.FormValue("description"))
	if desc == "" {
		http.Error(w, "describe it first — the draft needs something to work with", http.StatusBadRequest)
		return
	}
	yamlText, err := cl.Draft(r.FormValue("kind"), desc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, template.HTMLEscapeString(yamlText))
}

// validateDoc checks the editor's content and renders the report.
func validateDoc(cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	validationReport(cl, w, r.FormValue("kind"), r.FormValue("yaml"))
}

// saveDoc validates, then saves through the daemon. Validation problems
// render in the same target as the save result; an existing file comes
// back as 409 so the UI can ask and retry with overwrite confirmed.
func saveDoc(cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	content := r.FormValue("yaml")
	if strings.TrimSpace(content) == "" {
		http.Error(w, "nothing to save — the editor is empty", http.StatusBadRequest)
		return
	}
	res, err := cl.Validate(kind, content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !res.OK {
		renderIssues(w, res)
		return
	}
	overwrite := r.FormValue("overwrite") == "1"
	var path string
	if kind == "persona" {
		path, err = cl.SavePersona(r.FormValue("scope"), content, overwrite)
	} else {
		path, err = cl.Save(kind, content, overwrite)
	}
	if err != nil {
		// "already exists" is the daemon's refuse-until-confirmed answer
		// (409 drives the UI's overwrite dialog); anything else is a real
		// failure for the toast.
		if strings.Contains(err.Error(), "already exists") {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	next := `<a href="/">dashboard</a>`
	if kind == "persona" {
		next = `<a href="/personas">manage personas</a> · <a href="/">dashboard</a>`
	}
	fmt.Fprintf(w, `<p class="success">✓ saved <code>%s</code> — %s</p>`,
		template.HTMLEscapeString(path), next)
}

// validationReport writes the validate fragment: ✓, or every problem
// with its place in the document. False = content did not pass (or the
// daemon could not be asked) and the response is already written.
func validationReport(cl *daemon.Client, w http.ResponseWriter, kind, content string) bool {
	res, err := cl.Validate(kind, content)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return false
	}
	if res.OK {
		fmt.Fprint(w, `<p class="success">✓ valid — loop's parser accepts this document.</p>`)
		return true
	}
	renderIssues(w, res)
	return false
}

func renderIssues(w http.ResponseWriter, res daemon.ValidationResult) {
	fmt.Fprintf(w, `<p class="error">✗ %d problem(s):</p><ul class="issues">`, len(res.Errors))
	for _, is := range res.Errors {
		path := is.Path
		if path == "" {
			path = "document"
		}
		fmt.Fprintf(w, `<li><code>%s</code> — %s</li>`,
			template.HTMLEscapeString(path), template.HTMLEscapeString(is.Message))
	}
	fmt.Fprint(w, `</ul>`)
}
