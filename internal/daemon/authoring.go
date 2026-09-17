package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/generator"
	"github.com/shafi-/loop/internal/llm"
)

// Authoring: the daemon drafts documents with the same generator `loop
// new` uses, validates YAML with the same strict parser `loop validate`
// uses, and saves into the workspace's conventional directories. The
// web UI drives these endpoints; the daemon owns the workspace and the
// provider environment, so keys never leave this process.

// kindSpec is one authoring target: where its files live, the strict
// parser that gates every save (the same one `loop validate` uses), and
// the generator call that drafts it. parse receives the persona library
// (project first, then global) so room and pipeline references validate
// against what the workspace can actually resolve.
type kindSpec struct {
	dir    string
	noun   string
	parse  func(content []byte, lib *config.PersonaLibrary) (name string, err error)
	drafts func(g *generator.Generator, ctx context.Context, description string, personas []string) ([]byte, error)
}

// authoringLibrary is the persona library in scope order: the daemon
// workspace's personas/ first, then the user-wide ~/.loop/personas.
func authoringLibrary() *config.PersonaLibrary {
	dirs := []string{"personas"}
	if g, err := config.GlobalPersonaDir(); err == nil {
		dirs = append(dirs, g)
	}
	return config.NewPersonaLibrary(dirs...)
}

var kindSpecs = map[string]kindSpec{
	"pipeline": {
		dir:  "pipelines",
		noun: "pipeline",
		parse: func(b []byte, lib *config.PersonaLibrary) (string, error) {
			p, err := config.ParsePipeline(b)
			if err != nil {
				return "", err
			}
			if err := p.ResolvePersonas(lib); err != nil {
				return "", err
			}
			return p.Name, nil
		},
		drafts: func(g *generator.Generator, ctx context.Context, d string, _ []string) ([]byte, error) {
			r, err := g.Generate(ctx, d)
			if err != nil {
				return nil, err
			}
			return r.YAML, nil
		},
	},
	"room": {
		dir:  "rooms",
		noun: "room",
		parse: func(b []byte, lib *config.PersonaLibrary) (string, error) {
			r, err := config.ParseRoom(b)
			if err != nil {
				return "", err
			}
			if err := r.ResolvePersonas(lib); err != nil {
				return "", err
			}
			return r.Name, nil
		},
		drafts: func(g *generator.Generator, ctx context.Context, d string, personas []string) ([]byte, error) {
			r, err := g.GenerateRoom(ctx, d, personas)
			if err != nil {
				return nil, err
			}
			return r.YAML, nil
		},
	},
	"persona": {
		dir:  "personas",
		noun: "persona",
		parse: func(b []byte, _ *config.PersonaLibrary) (string, error) {
			p, err := config.ParsePersona(b)
			if err != nil {
				return "", err
			}
			return p.Name, nil
		},
	},
}

// envGenerator resolves the environment's provider (the grand rule, no
// flags) and returns a generator bound to it.
func envGenerator() (*generator.Generator, error) {
	rm := (&config.ModelConfig{}).Resolve()
	opts := llm.Options{BaseURL: rm.BaseURL, MaxAttempts: 3}
	opts.APIKey = os.Getenv(rm.APIKeyEnv)
	if opts.APIKey == "" && rm.BaseURL == "" {
		return nil, fmt.Errorf("no API key configured — set %s (or its base URL for a local server)", rm.APIKeyEnv)
	}
	p, err := llm.New(rm.Provider, opts)
	if err != nil {
		return nil, err
	}
	return &generator.Generator{Provider: p, ProviderName: rm.Provider, Model: rm.Model}, nil
}

// handleDraft turns a plain-language description into drafted YAML. The
// generator's honesty contract holds here too: a failure — provider or
// validation budget spent — comes back as an error, never a guess.
func (s *Server) handleDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind        string `json:"kind"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with \"kind\" and \"description\"")
		return
	}
	spec, ok := kindSpecs[req.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown kind %q (pipeline or room)", req.Kind)
		return
	}
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		writeError(w, http.StatusBadRequest, "empty description")
		return
	}
	if len(desc) > 8000 {
		desc = desc[:8000]
	}
	gen, err := s.newGenerator()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "drafting needs a provider: %v", err)
		return
	}
	// Room drafts can reference the workspace's library personas.
	var personas []string
	if req.Kind == "room" {
		personas = authoringLibrary().Names()
	}
	yaml, err := spec.drafts(gen, r.Context(), desc, personas)
	if err != nil {
		writeError(w, http.StatusBadGateway, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": string(yaml)})
}

// validationIssue is one problem on the wire: where in the document,
// and what's wrong with it.
type validationIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func validationIssues(err error) []validationIssue {
	if ve, ok := err.(config.ValidationErrors); ok {
		out := make([]validationIssue, 0, len(ve))
		for _, e := range ve {
			out = append(out, validationIssue{Path: e.Path, Message: e.Message})
		}
		return out
	}
	return []validationIssue{{Message: err.Error()}}
}

// handleValidate checks YAML with the document's own strict parser —
// exactly what `loop validate` would report for the saved file. A
// failed validation is a normal answer (200, ok=false), not an error.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with \"kind\" and \"content\"")
		return
	}
	spec, ok := kindSpecs[req.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown kind %q (pipeline, room, or persona)", req.Kind)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false,
			"errors": []validationIssue{{Message: "nothing to validate — the editor is empty"}}})
		return
	}
	if _, err := spec.parse([]byte(req.Content), authoringLibrary()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "errors": validationIssues(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// saveName turns a document's declared name into one safe filename
// component: lowercase, runs of anything outside a-z0-9-_/ _ collapsed
// to "-", trimmed, capped like ids. Nothing that survives this can
// escape the kind's directory.
var unsafeNameRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func saveName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = unsafeNameRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "untitled"
	}
	if len(s) > 64 {
		s = strings.Trim(s[:64], "-")
	}
	return s
}

// handleSave validates, then writes the file into the kind's directory.
// The name on disk always derives from the document itself (never the
// client), and overwriting requires the explicit flag — a 409 asks the
// UI to confirm first. Personas take a scope: "project" (the daemon's
// workspace) or "global" (~/.loop/personas, every project).
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind      string `json:"kind"`
		Scope     string `json:"scope,omitempty"`
		Content   string `json:"content"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON with \"kind\" and \"content\"")
		return
	}
	spec, ok := kindSpecs[req.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown kind %q (pipeline, room, or persona)", req.Kind)
		return
	}
	if req.Scope != "" && req.Kind != "persona" {
		writeError(w, http.StatusBadRequest, "scope only applies to personas")
		return
	}
	switch req.Scope {
	case "", "project", "global":
	default:
		writeError(w, http.StatusBadRequest, "unknown scope %q (project or global)", req.Scope)
		return
	}
	name, err := spec.parse([]byte(req.Content), authoringLibrary())
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  fmt.Sprintf("the %s does not validate — fix it and save again", spec.noun),
			"errors": validationIssues(err),
		})
		return
	}
	dir := spec.dir
	if req.Scope == "global" {
		if dir, err = config.GlobalPersonaDir(); err != nil {
			writeError(w, http.StatusInternalServerError, "resolving the global persona directory: %v", err)
			return
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "creating %s: %v", dir, err)
		return
	}
	path := filepath.Join(dir, saveName(name)+".yaml")
	if !req.Overwrite {
		if _, err := os.Stat(path); err == nil {
			writeError(w, http.StatusConflict, "%s already exists — save again to overwrite it", path)
			return
		}
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "writing %s: %v", path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}
