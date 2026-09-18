package webui

import (
	"html/template"
	"net/http"

	"github.com/shafi-/loop/internal/daemon"
)

// Persona management pages: the library listing (both scopes) with edit
// and delete, feeding the /new/persona authoring page.

func personasPage(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	personas, _ := cl.Personas()
	renderPage(tmpl, w, "personas", pageData{Title: "personas", Workspace: workspaceOf(cl), Personas: personas})
}

// fragPersonas re-renders the listing — the delete and refresh targets.
func fragPersonas(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter) {
	personas, _ := cl.Personas()
	if err := tmpl.ExecuteTemplate(w, "frag_personas", pageData{Personas: personas}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// personaDelete removes a persona; the daemon refuses (409) while
// workspace documents still reference it, which surfaces as a toast.
func personaDelete(tmpl *template.Template, cl *daemon.Client, w http.ResponseWriter, r *http.Request) {
	if _, err := cl.DeletePersona(r.FormValue("scope"), r.FormValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	fragPersonas(tmpl, cl, w)
}
