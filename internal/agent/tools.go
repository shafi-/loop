// Room-agent tools: a small native executor so conversation agents can
// persist files without the cline runtime. Pipeline agent stages keep
// their heavier executor-backed loop; this is the lightweight
// counterpart for the room surface — same tool names, same semantics,
// executed in-process with a path guard and full audit.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/knowledge"
	"github.com/shafi-/loop/internal/llm"
)

// MaxToolRounds bounds the agentic loop inside one reply: a room agent
// that never stops calling tools must not monopolize the turn.
const MaxToolRounds = 8

// commandTimeout keeps run_command from hanging an interactive room.
const commandTimeout = 60 * time.Second

// maxToolOutput caps tool output fed back into the conversation.
const maxToolOutput = 16 << 10 // 16 KiB

// roomToolDefs describes the native tool set in provider-neutral form.
var roomToolDefs = map[string]llm.ToolDef{
	"read_file": {
		Name:        "read_file",
		Description: "Read a file in the workspace.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
	},
	"write_file": {
		Name: "write_file",
		Description: "Write a plan file under .loop/plans/ (the only writes a " +
			"room allows — code changes belong to a pipeline run).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	},
	"run_command": {
		Name: "run_command",
		Description: "Run a shell command in the workspace (60s timeout). " +
			"For validation — checks, tests, builds, running the program — not for changing code.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	},
	"project_notes": {
		Name: "project_notes",
		Description: "Read the maintained project knowledge (area notes). Pass an " +
			"empty slug to list the available notes; pass a note's slug to read it.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug": map[string]any{
					"type":        "string",
					"description": "Area note slug; empty string lists all notes",
				},
			},
			"required": []string{"slug"},
		},
	},
}

// toolDefsFor returns definitions for the requested tool names, in order.
// project_notes rides along automatically: it is read-only, and the
// knowledge layer's whole point is that any tool-using agent can pull a
// summary instead of re-reading the codebase.
func toolDefsFor(names []string) []llm.ToolDef {
	var defs []llm.ToolDef
	hasNotes := false
	for _, n := range names {
		d, ok := roomToolDefs[n]
		if !ok {
			continue
		}
		if n == "project_notes" {
			hasNotes = true
		}
		defs = append(defs, d)
	}
	if len(defs) > 0 && !hasNotes {
		defs = append(defs, roomToolDefs["project_notes"])
	}
	return defs
}

// RoomTools returns the names of the native room tools, in canonical order.
func RoomTools() []string {
	return []string{"read_file", "write_file", "run_command", "project_notes"}
}

// execRoomTool runs one tool call and returns the text fed back to the
// model. cwd is the room's working directory; file paths are confined
// to it (absolute paths and `..` escapes are refused).
func execRoomTool(ctx context.Context, name, argsJSON, cwd string) (string, error) {
	switch name {
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("read_file: arguments must be JSON with a path: %w", err)
		}
		p, err := safePath(cwd, a.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read_file %s: %w", a.Path, err)
		}
		return clip(string(data)), nil

	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("write_file: arguments must be JSON with path and content: %w", err)
		}
		p, err := safePath(cwd, a.Path)
		if err != nil {
			return "", err
		}
		// The room is for planning: a chat persona writes plans, never
		// the project. Enforced here — deterministically, not by prompt
		// discipline — so the room cannot mutate code no matter what the
		// model decides; implementation belongs to a gated pipeline run.
		if !inPlansDir(cwd, p) {
			return "refused: the room is for planning — write_file may only write plans under .loop/plans/. " +
				"Write the agreed plan there, or discuss the change and let a pipeline run (/run implement) make it — then reply.", nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", fmt.Errorf("write_file %s: %w", a.Path, err)
		}
		if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
			return "", fmt.Errorf("write_file %s: %w", a.Path, err)
		}
		return fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(a.Content)), nil

	case "run_command":
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("run_command: arguments must be JSON with a command: %w", err)
		}
		// Validation commands only — the same planning-room rule the
		// write_file guard enforces, applied to the shell's escape
		// hatches. Deterministic, so "never change code from the room"
		// does not depend on the model's discipline.
		if refusal := validateRoomCommand(a.Command); refusal != "" {
			return refusal, nil
		}
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		out, err := exec.CommandContext(cctx, "sh", "-c", a.Command).CombinedOutput()
		text := clip(string(out))
		if err != nil {
			return text, fmt.Errorf("run_command %q: %w", a.Command, err)
		}
		return text, nil
	case "project_notes":
		var a struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "", fmt.Errorf("project_notes: arguments must be JSON with a slug (empty to list): %w", err)
		}
		return projectNotesText(cwd, strings.TrimSpace(a.Slug)), nil

	}
	return "", fmt.Errorf("unknown tool %q (available: read_file, write_file, run_command, project_notes)", name)
}

// projectNotesText renders the knowledge layer for the model: the index
// when no slug is given, one note's body otherwise. Read-only over the
// workspace's .loop/knowledge/ — no path guard needed beyond the store's
// own slug validation.
func projectNotesText(cwd, slug string) string {
	if slug == "" {
		idx, err := knowledge.Load(cwd)
		if err != nil || idx == nil || len(idx.Notes) == 0 {
			return "no project notes yet — the digest and area notes appear after the first implementation turn (or run: loop digest)"
		}
		var b strings.Builder
		if _, ok := knowledge.ReadDigest(cwd); ok {
			b.WriteString("digest: present (you already carry it in your context)\n")
		}
		b.WriteString("area notes:\n")
		for _, n := range idx.Notes {
			fmt.Fprintf(&b, "  %s — %s: %s\n", n.Slug, n.Title, n.Scope)
		}
		b.WriteString("read one with project_notes <slug>")
		return b.String()
	}
	body, ok := knowledge.ReadNote(cwd, slug)
	if !ok {
		return fmt.Sprintf("no note %q — call project_notes with an empty slug to list what exists", slug)
	}
	return "note " + slug + " (maintained summary — verify against code):\n" + body
}

// safePath confines p inside cwd: no absolute paths, no `..` escapes.
func safePath(cwd, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative to the workspace", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	if cwd == "" {
		return clean, nil
	}
	return filepath.Join(cwd, clean), nil
}

// inPlansDir reports whether p (already workspace-confined) sits inside
// the workspace's .loop/plans/ — the only place a chat room writes.
func inPlansDir(cwd, p string) bool {
	plans := filepath.Join(cwd, ".loop", "plans")
	rel, err := filepath.Rel(plans, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateRoomCommand refuses the shell's escape hatches from a chat
// room: heredocs and interpreter inline scripts (arbitrary code — the
// route around the write_file guard), output redirection into files,
// and the plain mutating utilities. Rooms run validation — checks,
// tests, builds, running the program; not `2>/dev/null`-style fd
// redirects (usually), and nothing that edits.
// Not a security boundary (a validation command's own program can still
// do anything) — it is the room-behavior contract, enforced.
func validateRoomCommand(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return ""
	}
	const why = "refused: rooms run validation commands only (checks, tests, builds, running the program) — " +
		"this command writes files or runs inline scripts. Code changes belong to a pipeline run (/run implement)."
	if strings.Contains(trimmed, "<<") {
		return why // heredoc: an inline script
	}
	fields := strings.Fields(stripQuoted(trimmed))
	if len(fields) > 0 {
		if _, scripted := interpreters[fields[0]]; scripted {
			for _, f := range fields[1:] {
				if f == "-c" || f == "-e" || strings.HasPrefix(f, "-c=") {
					return why // interpreter inline script
				}
			}
		}
	}
	for _, f := range fields {
		if mutating[f] {
			return why
		}
		if f == "sed" {
			for _, g := range fields[1:] {
				if g == "-i" || strings.HasPrefix(g, "-i") {
					return why // in-place edit (also -i.bak, -in)
				}
			}
		}
	}
	if writesFiles(stripQuoted(trimmed)) {
		return why
	}
	return ""
}

// interpreters whose -c/-e flags mean "run this string as code".
var interpreters = map[string]bool{
	"python": true, "python3": true, "python2": true, "perl": true,
	"ruby": true, "node": true, "nodejs": true, "php": true,
	"bash": true, "sh": true, "zsh": true, "ksh": true, "dash": true,
	"lua": true, "osascript": true,
}

// mutating utilities — never needed to validate a change.
var mutating = map[string]bool{
	"rm": true, "mv": true, "cp": true, "dd": true, "tee": true,
	"chmod": true, "chown": true, "truncate": true, "shred": true,
	"mkfs": true, "ln": true,
}

// stripQuoted removes single- and double-quoted spans (naively — no
// nesting) so operators inside strings don't trip the scanner.
func stripQuoted(s string) string {
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if c == q {
				q = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			q = c
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// writesFiles reports whether the (quote-stripped) command redirects
// output into a file: `>` or `>>`, except fd dups (`2>&1`) and fd
// redirects (`2>/dev/null`), which validation commands legitimately use.
func writesFiles(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '>' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '>' {
			return true // >>
		}
		prev, next := byte(0), byte(0)
		if i > 0 {
			prev = s[i-1]
		}
		if i+1 < len(s) {
			next = s[i+1]
		}
		if next == '&' || next == '(' || next == '=' {
			continue // >& fd dup, >(...) substitution, >= test
		}
		if prev >= '1' && prev <= '9' {
			continue // 2> target: fd redirect
		}
		return true // > file
	}
	return false
}

// clip bounds tool output fed back to the model.
func clip(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + "\n… (truncated)"
}
