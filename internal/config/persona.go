package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The persona library: named agent identities stored as YAML files, one
// per file, referenceable from rooms (agents) and pipelines (personas)
// with a `persona: <name>` entry. Two scopes exist:
//
//   - project:  <workspace>/personas/   — available in this project only
//   - global:   ~/.loop/personas/       — available in every project
//
// Project personas shadow global ones with the same name.

// ParsePersona validates persona-library bytes and returns the persona.
func ParsePersona(data []byte) (*Persona, error) {
	var p Persona
	if err := Decode(data, &p); err != nil {
		return nil, err
	}
	if errs := validatePersona(p); len(errs) > 0 {
		return nil, ValidationErrors(errs)
	}
	return &p, nil
}

// LoadPersona reads and validates one persona-library file.
func LoadPersona(path string) (*Persona, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParsePersona(data)
	if err != nil {
		return nil, fmt.Errorf("persona %s: %w", filepath.Base(path), err)
	}
	return p, nil
}

func validatePersona(p Persona) ValidationErrors {
	var errs ValidationErrors
	err := func(path, format string, args ...any) {
		errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if p.Name == "" {
		err("name", "is required")
	} else if !validIdent(p.Name) {
		err("name", "must be lowercase letters, digits, '-' or '_' (got %q)", p.Name)
	}
	if p.Ref != "" {
		err("persona", "a persona file is a concrete persona — \"persona:\" references belong in rooms and pipelines")
	}
	if p.System == "" {
		err("system", "is required")
	}
	for _, tool := range p.Tools {
		if !allowedRoomTools[tool] {
			err("tools", "%q is not a known tool (available: read_file, write_file, run_command)", tool)
		}
	}
	validateModel(err, "model", p.Model)
	return errs
}

// personaEntry is one library slot: the persona, or — for a file that
// exists but doesn't parse — the reason it can't serve.
type personaEntry struct {
	persona *Persona
	path    string
	err     error
}

// PersonaLibrary is the set of personas a document may reference. The
// directories are given in precedence order: the first directory that
// knows a name wins, so project personas shadow global ones.
type PersonaLibrary struct {
	byName map[string]personaEntry
	dirs   []string
}

// NewPersonaLibrary loads the given directories in precedence order.
// Missing directories are empty, not errors.
func NewPersonaLibrary(dirs ...string) *PersonaLibrary {
	lib := &PersonaLibrary{byName: map[string]personaEntry{}, dirs: dirs}
	for _, dir := range dirs {
		lib.addDir(dir)
	}
	return lib
}

func (l *PersonaLibrary) addDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			continue
		}
		path := filepath.Join(dir, n)
		p, err := LoadPersona(path)
		if err != nil {
			// Remember broken files by filename stem: a document that
			// references that name gets the parse error, not a bare
			// "unknown persona". Unreferenced broken files stay quiet.
			stem := strings.TrimSuffix(strings.TrimSuffix(n, ".yml"), ".yaml")
			if _, exists := l.byName[stem]; !exists {
				l.byName[stem] = personaEntry{path: path, err: err}
			}
			continue
		}
		if _, exists := l.byName[p.Name]; !exists {
			l.byName[p.Name] = personaEntry{persona: p, path: path}
		}
	}
}

// Lookup resolves one name. Unknown references are errors: a document
// that silently runs without an agent it names is worse than one that
// refuses to load.
func (l *PersonaLibrary) Lookup(name string) (*Persona, error) {
	if e, ok := l.byName[name]; ok {
		if e.err != nil {
			return nil, fmt.Errorf("persona %q: %s is not a valid persona file (%v)", name, e.path, e.err)
		}
		return e.persona, nil
	}
	return nil, fmt.Errorf("unknown persona %q (looked in: %s)", name, strings.Join(l.dirs, ", "))
}

// Names lists the loadable persona names, alphabetically.
func (l *PersonaLibrary) Names() []string {
	var out []string
	for name, e := range l.byName {
		if e.err == nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// GlobalPersonaDir is the user-wide persona library — personas saved
// there are available to every project on the machine.
func GlobalPersonaDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loop", "personas"), nil
}

// PersonaDirsFor returns the library directories for a document at
// path, in precedence order: personas/ next to the document, personas/
// in its parent directory (the workspace root for rooms/x.yaml), then
// the global library.
func PersonaDirsFor(documentPath string) []string {
	dir := filepath.Dir(documentPath)
	dirs := []string{
		filepath.Join(dir, "personas"),
		filepath.Join(filepath.Dir(dir), "personas"),
	}
	if g, err := GlobalPersonaDir(); err == nil {
		dirs = append(dirs, g)
	}
	return dirs
}

// PersonaFileInfo describes one persona file found on disk: the parsed
// persona, or Err when the file exists but doesn't validate.
type PersonaFileInfo struct {
	Persona
	Path string
	Err  error
}

// ScanPersonaDir lists the persona files of one directory, sorted by
// name. Missing directories scan as empty.
func ScanPersonaDir(dir string) []PersonaFileInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []PersonaFileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			continue
		}
		path := filepath.Join(dir, n)
		p, err := LoadPersona(path)
		if err != nil {
			out = append(out, PersonaFileInfo{Path: path, Err: err})
			continue
		}
		out = append(out, PersonaFileInfo{Persona: *p, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ResolvePersonas fills the room's persona references from the library,
// in place. Reference entries may specialize the library persona with
// their own tools or model block; everything else comes from the
// library.
func (r *Room) ResolvePersonas(lib *PersonaLibrary) error {
	for i := range r.Agents {
		if r.Agents[i].Ref == "" {
			continue
		}
		if err := resolveRef(&r.Agents[i], lib); err != nil {
			return fmt.Errorf("agents[%d]: %w", i, err)
		}
	}
	return nil
}

// ResolvePersonas fills the pipeline's persona references, in place.
func (p *Pipeline) ResolvePersonas(lib *PersonaLibrary) error {
	for i := range p.Personas {
		if p.Personas[i].Ref == "" {
			continue
		}
		if err := resolveRef(&p.Personas[i], lib); err != nil {
			return fmt.Errorf("personas[%d]: %w", i, err)
		}
	}
	return nil
}

func resolveRef(target *Persona, lib *PersonaLibrary) error {
	src, err := lib.Lookup(target.Ref)
	if err != nil {
		return err
	}
	tools, model := target.Tools, target.Model
	*target = *src
	target.Ref = ""
	if tools != nil {
		target.Tools = tools
	}
	if model != nil {
		target.Model = model
	}
	return nil
}
