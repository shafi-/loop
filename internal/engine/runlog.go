package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shafi-/loop/internal/usage"
)

// RunLog persists one run: the exact pipeline it ran, an append-only
// event stream, and a context snapshot after every completed stage. This
// is what makes runs auditable and resumable.
type RunLog struct {
	Dir string
	f   *os.File
}

// runDirBase is where runs are stored relative to the workspace.
const runDirBase = ".loop/runs"

// newRunID generates a sortable, collision-resistant run identifier.
func newRunID() string {
	return fmt.Sprintf("%s-%04x", time.Now().Format("20060102-150405"), time.Now().UnixNano()&0xffff)
}

// NewRunID exposes id generation for the CLI so it can display the id
// before handing control to the runner.
func NewRunID() string { return newRunID() }

// CreateRunLog makes the run directory and snapshots the pipeline source.
func CreateRunLog(runsDir, runID string, pipelineSource []byte) (*RunLog, error) {
	if runsDir == "" {
		runsDir = runDirBase
	}
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml"), pipelineSource, 0o644); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &RunLog{Dir: dir, f: f}, nil
}

// OpenRunLog opens an existing run for resume (events append, not truncate).
func OpenRunLog(runsDir, runID string) (*RunLog, error) {
	if runsDir == "" {
		runsDir = runDirBase
	}
	dir := filepath.Join(runsDir, runID)
	if _, err := os.Stat(filepath.Join(dir, "pipeline.yaml")); err != nil {
		return nil, fmt.Errorf("run %q not found under %s", runID, runsDir)
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &RunLog{Dir: dir, f: f}, nil
}

// readPipelineSnapshot returns the exact pipeline bytes this run started from.
func (l *RunLog) readPipelineSnapshot() ([]byte, error) {
	return os.ReadFile(filepath.Join(l.Dir, "pipeline.yaml"))
}

// runState is the resume pointer for a run: the exact execution path
// (including rework-loop revisits), the stop point (a failed or paused
// stage — resume re-runs it), and whether the run finished. Order
// matters — a router can send the run backwards, so "first stage
// without output" is meaningless; the path is the truth.
type runState struct {
	Path   []string `json:"path"`
	Failed string   `json:"failed,omitempty"`
	Paused string   `json:"paused,omitempty"`
	Done   bool     `json:"done,omitempty"`
	// Usage accumulates every session's tokens (resume adds onto it);
	// nil on runs that predate the ledger.
	Usage *usage.Total `json:"usage,omitempty"`
}

// usageTotalPtr is a nil-safe pointer for runState.Usage.
func usageTotalPtr(t usage.Total) *usage.Total { return &t }

// SaveState atomically persists the resume pointer.
func (l *RunLog) SaveState(s runState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.Dir, "state.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.Dir, "state.json"))
}

// LoadState reads the resume pointer. It returns a nil state when the run
// predates state recording.
func LoadState(runsDir, runID string) (*runState, error) {
	if runsDir == "" {
		runsDir = runDirBase
	}
	data, err := os.ReadFile(filepath.Join(runsDir, runID, "state.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s runState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("corrupt state in run %s: %w", runID, err)
	}
	return &s, nil
}

// Event appends one structured event to events.jsonl.
func (l *RunLog) Event(eventType, stage string, data map[string]any) {
	entry := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"type":  eventType,
		"stage": stage,
	}
	for k, v := range data {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return // a broken telemetry line must never fail a run
	}
	l.f.Write(append(line, '\n'))
}

// SaveContext writes the full context snapshot (atomic replace so a crash
// mid-write can never corrupt the resume point).
func (l *RunLog) SaveContext(c *Context) error {
	data, err := json.MarshalIndent(c.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.Dir, "context.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.Dir, "context.json"))
}

// LoadContext reads the context snapshot for resume. It returns nil when
// no snapshot exists (fresh run).
func LoadContext(runsDir, runID string) (*Context, error) {
	if runsDir == "" {
		runsDir = runDirBase
	}
	path := filepath.Join(runsDir, runID, "context.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap map[string]any
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("corrupt context snapshot in run %s: %w", runID, err)
	}
	return Restore(snap), nil
}

// Close flushes and releases the events file.
func (l *RunLog) Close() error { return l.f.Close() }
