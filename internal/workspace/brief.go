// Package workspace assembles the project brief that grounds room
// agents in the workspace they were hosted from: name, detected stack,
// a README excerpt, and a capped layout listing. Everything is
// deterministic file reading — no subprocesses, no model calls — and
// every section is optional: an empty workspace yields an empty brief.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Caps keep the brief a context, not a dump: it rides in every agent's
// system prompt for the life of the room.
const (
	maxEntries  = 20        // depth-1 layout entries
	maxReadme   = 1200      // README excerpt characters
	maxBrief    = 3 * 1024  // whole-brief ceiling
	maxReadSize = 64 * 1024 // never read more of any marker/readme
	noiseMax    = ".git .hg .svn node_modules vendor dist build target __pycache__ .loop .idea .vscode coverage"
)

// skipName reports whether a root entry stays out of the layout line:
// dotfiles, secrets, and the usual build/dependency noise.
func skipName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	if name == ".env" || strings.EqualFold(name, "license") {
		return true
	}
	for _, n := range strings.Fields(noiseMax) {
		if name == n {
			return true
		}
	}
	return false
}

// IsNoise reports whether a directory or file name is excluded from
// briefs and inventories: dotfiles, secrets, and the usual
// build/dependency noise. Shared with the knowledge layer, which walks
// deeper than the depth-1 layout but must skip the same things.
func IsNoise(name string) bool { return skipName(name) }

// Markers returns the root files the brief's facts come from — the
// detected stack marker and the README — so staleness checks can watch
// exactly the files a digest quotes. Absent files are omitted.
func Markers(dir string) []string {
	var out []string
	if detectStack(dir) != "" {
		for _, m := range stackMarkers {
			if _, err := os.Stat(filepath.Join(dir, m.file)); err == nil {
				out = append(out, m.file)
				break
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if strings.EqualFold(n, "readme.md") || strings.EqualFold(n, "readme.markdown") || strings.EqualFold(n, "readme.txt") {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// stackMarker is one root file that names the project's stack.
type stackMarker struct {
	file string
	// render turns the marker's bytes (already size-capped) into the
	// stack description; nil bytes handling means the marker needs no
	// content (Gemfile, requirements.txt).
	render func(data []byte) string
}

// nameRe reads a project name from marker files of several dialects:
// line-anchored (YAML/TOML: `name: x` / `name = "x"`) and inline JSON
// (`{"name": "x"` on a minified single line).
var nameRe = regexp.MustCompile(`(?m)(?:^|[{,])\s*"?name"?\s*[:=]\s*"?([^\s"',]+)"?`)

// moduleRe reads go.mod's `module <path>` — the file has no name key.
var moduleRe = regexp.MustCompile(`(?m)^module\s+(\S+)`)

func packageName(data []byte) string {
	if m := nameRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

var stackMarkers = []stackMarker{
	{"go.mod", func(b []byte) string {
		name := ""
		if m := moduleRe.FindSubmatch(b); m != nil {
			name = string(m[1])
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
		}
		if name != "" {
			return "Go (module " + name + ")"
		}
		return "Go"
	}},
	{"package.json", func(b []byte) string {
		if n := packageName(b); n != "" {
			return "Node (" + n + ")"
		}
		return "Node"
	}},
	{"Cargo.toml", func(b []byte) string {
		if n := packageName(b); n != "" {
			return "Rust (crate " + n + ")"
		}
		return "Rust"
	}},
	{"pyproject.toml", func(b []byte) string {
		if n := packageName(b); n != "" {
			return "Python (" + n + ")"
		}
		return "Python"
	}},
	{"requirements.txt", func(b []byte) string { return "Python" }},
	{"Gemfile", func(b []byte) string { return "Ruby" }},
	{"composer.json", func(b []byte) string { return "PHP (composer)" }},
	{"pom.xml", func(b []byte) string { return "Java (Maven)" }},
	{"build.gradle", func(b []byte) string { return "Java (Gradle)" }},
	{"mix.exs", func(b []byte) string { return "Elixir" }},
}

// detectStack finds the first marker present and describes it.
func detectStack(dir string) string {
	for _, m := range stackMarkers {
		data, err := os.ReadFile(filepath.Join(dir, m.file))
		if err != nil {
			continue
		}
		return m.render(data)
	}
	return ""
}

// readmeExcerpt pulls the README's first heading and first paragraph —
// the project's own words about itself, not a summary we invent.
func readmeExcerpt(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(e.Name(), "readme.md") &&
			!strings.EqualFold(e.Name(), "readme.markdown") &&
			!strings.EqualFold(e.Name(), "readme.txt") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || len(data) > maxReadSize {
			return ""
		}
		return excerpt(string(data), maxReadme)
	}
	return ""
}

// excerpt keeps the README's opening — its first heading(s) plus its
// first paragraph of running text — capped at limit characters.
func excerpt(md string, limit int) string {
	lines := strings.Split(md, "\n")
	var out []string
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue // blank between heading and paragraph
		}
		if strings.HasPrefix(t, "```") {
			break // code fences are not a summary
		}
		out = append(out, t)
		// A text line (not a heading) ends the excerpt when the next
		// line is blank, a fence, or the end of the file: that's the
		// first paragraph complete.
		if !strings.HasPrefix(t, "#") {
			var next string
			if i+1 < len(lines) {
				next = strings.TrimSpace(lines[i+1])
			}
			if next == "" || strings.HasPrefix(next, "```") {
				break
			}
		}
		if len(strings.Join(out, " ")) >= limit {
			break
		}
	}
	s := strings.Join(out, " ")
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// layout lists depth-1 entries (dirs marked with /), noise skipped,
// capped — a map of the room's world, not a tree dump.
func layout(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if skipName(e.Name()) {
			continue
		}
		n := e.Name()
		if e.IsDir() {
			n += "/"
		}
		names = append(names, n)
		if len(names) >= maxEntries {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "  ")
}

// Brief renders the workspace context block for room agents. Empty
// string when the directory says nothing (the caller skips the block).
func Brief(dir string) string {
	if dir == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Project context — you are discussing THIS project, the workspace this room was opened in.\n")
	if stack := detectStack(dir); stack != "" {
		fmt.Fprintf(&b, "- Stack: %s\n", stack)
	}
	if r := readmeExcerpt(dir); r != "" {
		fmt.Fprintf(&b, "- Readme: %s\n", r)
	}
	if l := layout(dir); l != "" {
		fmt.Fprintf(&b, "- Layout: %s\n", l)
	}
	// A brief with only the header line carries no information; treat
	// it as no brief at all.
	out := strings.TrimSpace(b.String())
	if !strings.Contains(out, "\n- ") {
		return ""
	}
	if len(out) > maxBrief {
		out = out[:maxBrief] + "…"
	}
	return out
}
