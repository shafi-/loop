// Package daemon is loop's core engine running as a server: one process
// (`loop serve`) owns every run's child process and stdin, and exposes a
// small JSON API over a unix socket. The CLI (--daemon attach mode) and
// the web UI are both clients of it — which is what makes a gate asked
// by any run answerable from any surface.
//
// The daemon persists nothing of its own: .loop/runs/ remains the
// durable record, so daemon crash recovery is literally `--resume`.
package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// EventLine is one events.jsonl entry with its 1-based line number.
// Seq is the offset clients pass back for incremental polling ("give me
// everything after N").
type EventLine struct {
	Seq   int             `json:"seq"`
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event"`
}

// eventsPath is a run's event log under the daemon's runs dir.
func eventsPath(runsDir, runID string) string {
	return filepath.Join(runsDir, runID, "events.jsonl")
}

// readEvents returns events after the given sequence number (lines are
// numbered from 1; after=0 means everything). A missing file is not an
// error — the child may not have created its run dir yet.
func readEvents(path string, after int) ([]EventLine, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []EventLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	seq := 0
	for sc.Scan() {
		seq++
		if seq <= after {
			continue
		}
		line := sc.Bytes()
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(line, &probe) // a torn last line yields "" and is skipped below
		if probe.Type == "" {
			continue
		}
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		out = append(out, EventLine{Seq: seq, Type: probe.Type, Event: raw})
	}
	return out, sc.Err()
}

// countEvents returns the current number of event lines, so a client can
// attach to a stream without replaying history. Missing file → 0.
func countEvents(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}
