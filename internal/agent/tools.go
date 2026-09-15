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
		Name:        "write_file",
		Description: "Write (create or replace) a file in the workspace.",
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
		Name:        "run_command",
		Description: "Run a shell command in the workspace (60s timeout).",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []string{"command"},
		},
	},
}

// toolDefsFor returns definitions for the requested tool names, in order.
func toolDefsFor(names []string) []llm.ToolDef {
	var defs []llm.ToolDef
	for _, n := range names {
		if d, ok := roomToolDefs[n]; ok {
			defs = append(defs, d)
		}
	}
	return defs
}

// RoomTools returns the names of the native room tools, in canonical order.
func RoomTools() []string { return []string{"read_file", "write_file", "run_command"} }

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
		cctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		out, err := exec.CommandContext(cctx, "sh", "-c", a.Command).CombinedOutput()
		text := clip(string(out))
		if err != nil {
			return text, fmt.Errorf("run_command %q: %w", a.Command, err)
		}
		return text, nil
	}
	return "", fmt.Errorf("unknown tool %q (available: read_file, write_file, run_command)", name)
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

// clip bounds tool output fed back to the model.
func clip(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + "\n… (truncated)"
}
