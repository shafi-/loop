// Package cline implements the executor.Executor interface against the
// Cline agent runtime (@cline/sdk) via a Node sidecar host. The Go engine
// owns determinism and auditability; the host owns the agentic loop.
package cline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nerddevsltd/loop/internal/executor"
)

// MinNodeMajor is the SDK's documented floor (Node 22).
const MinNodeMajor = 22

// Executor runs tasks through the cline host process.
type Executor struct {
	NodeBin  string // default: LOOP_NODE env, else "node"
	HostPath string // default: ResolveHostPath()
}

// New builds an executor with environment-driven defaults.
func New() *Executor {
	node := os.Getenv("LOOP_NODE")
	if node == "" {
		node = "node"
	}
	return &Executor{NodeBin: node, HostPath: ResolveHostPath()}
}

// Name implements executor.Executor.
func (e *Executor) Name() string { return "cline" }

// ResolveHostPath finds the host script: explicit env override, then the
// install target, then a repo-local dev checkout.
func ResolveHostPath() string {
	if p := os.Getenv("LOOP_CLINE_HOST"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".loop", "executors", "cline", "index.mjs")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat(filepath.Join("host", "loop-cline-host", "index.mjs")); err == nil {
		return filepath.Join("host", "loop-cline-host", "index.mjs")
	}
	// No file exists; return the canonical install path so error messages
	// point where `loop executor install cline` would put it.
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loop", "executors", "cline", "index.mjs")
}

// Check verifies prerequisites and returns actionable problems. Doctor
// surfaces all of them; Run fails fast on the first.
func (e *Executor) Check() []error {
	var problems []error
	if major, ok := e.probeNodeMajor(); !ok {
		problems = append(problems, fmt.Errorf("node not found or unrecognized (%s): install Node.js %d+ — https://nodejs.org", e.NodeBin, MinNodeMajor))
	} else if major < MinNodeMajor {
		problems = append(problems, fmt.Errorf("node %d is too old: the cline executor needs Node %d+", major, MinNodeMajor))
	}
	if _, err := os.Stat(e.HostPath); err != nil {
		problems = append(problems, fmt.Errorf("host script missing at %s: run `loop executor install cline`", e.HostPath))
	} else if _, err := os.Stat(filepath.Join(filepath.Dir(e.HostPath), "node_modules", "@cline", "sdk")); err != nil {
		problems = append(problems, fmt.Errorf("@cline/sdk not installed: run `loop executor install cline` (or npm install in %s)", filepath.Dir(e.HostPath)))
	}
	return problems
}

// probeNodeMajor asks the configured binary for its version and parses it
// only when the answer looks like Node's ("v24.12.0"). Shells and other
// binaries print different shapes; an unconfirmed version is not an error
// here — callers decide whether to proceed (Run) or warn (doctor).
func (e *Executor) probeNodeMajor() (int, bool) {
	out, err := exec.Command(e.NodeBin, "--version").Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	if !strings.HasPrefix(s, "v") {
		return 0, false
	}
	n, err := parseNodeVersion(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseNodeVersion reads "v22.3.1" (or "22.3.1") into the major version.
func parseNodeVersion(s string) (int, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	major, _, ok := strings.Cut(s, ".")
	if !ok {
		return 0, fmt.Errorf("unrecognized node version output %q", s)
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("unrecognized node version output %q", s)
	}
	return n, nil
}

// taskWire is the protocol line sent to the host.
type taskWire struct {
	Type        string    `json:"type"`
	TaskID      string    `json:"taskId"`
	Instruction string    `json:"instruction"`
	System      string    `json:"system,omitempty"`
	CWD         string    `json:"cwd,omitempty"`
	Model       modelWire `json:"model"`
	Tools       []string  `json:"tools,omitempty"`
	MaxTurns    int       `json:"maxTurns,omitempty"`
	Approval    string    `json:"approval,omitempty"`
}

type modelWire struct {
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	APIKey      string   `json:"apiKey,omitempty"`
	BaseURL     string   `json:"baseUrl,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"`
}

// hostEvent is any protocol line the host sends.
type hostEvent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Tool    string `json:"tool"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
	Output  string `json:"output"`
}

// Run implements executor.Executor: spawn the host, stream its events,
// collect the final output.
func (e *Executor) Run(ctx context.Context, task executor.Task, onEvent func(executor.Event)) (*executor.Result, error) {
	// Preconditions: the host must exist (with an install hint if not);
	// a confirmed-but-too-old node is refused with a friendly message.
	// An unconfirmable interpreter is left to the spawn error.
	if _, err := os.Stat(e.HostPath); err != nil {
		return nil, fmt.Errorf("cline host missing at %s: run `loop executor install cline`", e.HostPath)
	}
	if major, ok := e.probeNodeMajor(); ok && major < MinNodeMajor {
		return nil, fmt.Errorf("node %d is too old: the cline executor needs Node %d+", major, MinNodeMajor)
	}

	wire := taskWire{
		Type:        "task",
		TaskID:      newTaskID(),
		Instruction: task.Instruction,
		System:      task.System,
		CWD:         task.CWD,
		Model: modelWire{
			Provider:    task.Model.Provider,
			Model:       task.Model.Model,
			APIKey:      task.Model.APIKey,
			BaseURL:     task.Model.BaseURL,
			Temperature: task.Model.Temperature,
			MaxTokens:   task.Model.MaxTokens,
		},
		Tools:    task.Tools,
		MaxTurns: task.MaxTurns,
		Approval: task.Approval,
	}
	line, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, e.NodeBin, e.HostPath)
	if task.CWD != "" {
		cmd.Dir = task.CWD
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{buf: &stderr, limit: 8192}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting cline host: %w", err)
	}
	// The host exits when stdin closes; if it doesn't, Wait's error is
	// secondary to whatever we already learned from its events.
	defer func() { stdin.Close(); cmd.Wait() }()

	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("sending task to host: %w", err)
	}
	stdin.Close() // one task per host invocation

	result := &executor.Result{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev hostEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // tolerate stray output; the protocol is line-JSON
		}
		switch ev.Type {
		case "text":
			if onEvent != nil {
				onEvent(executor.Event{Type: executor.EventText, Text: ev.Text})
			}
			result.Output += ev.Text
		case "tool_call":
			if onEvent != nil {
				onEvent(executor.Event{Type: executor.EventToolCall, Tool: ev.Tool, Detail: ev.Detail})
			}
		case "tool_result":
			if onEvent != nil {
				onEvent(executor.Event{Type: executor.EventToolResult, Tool: ev.Tool, Detail: ev.Detail})
			}
		case "notice":
			if onEvent != nil {
				onEvent(executor.Event{Type: executor.EventNotice, Text: ev.Text})
			}
		case "done":
			// Authoritative final output replaces streamed accumulation
			// (which may have missed non-delta text).
			if ev.Output != "" {
				result.Output = ev.Output
			}
			return result, nil
		case "error":
			return nil, fmt.Errorf("cline host: %s", ev.Message)
		}
	}
	// Stream ended without a done/error line.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tail := strings.TrimSpace(stderr.String())
	if tail != "" {
		return nil, fmt.Errorf("cline host exited without completing; stderr: %s", tailLines(tail, 3))
	}
	return nil, fmt.Errorf("cline host exited without completing")
}

// limitedWriter keeps at most limit bytes, like the engine's cappedBuffer.
type limitedWriter struct {
	buf   *strings.Builder
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func newTaskID() string {
	return fmt.Sprintf("t%d", time.Now().UnixNano())
}
