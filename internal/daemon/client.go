package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client speaks to a loop daemon over its unix socket. Shared by the
// CLI's --daemon attach mode and (next) the web UI, so every surface
// drives runs through the same API.
type Client struct {
	http *http.Client
	base string
}

// DefaultSocket is where `loop serve` listens unless overridden
// ($LOOP_DAEMON_SOCK or --socket).
func DefaultSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loop", "daemon.sock"), nil
}

// SocketPath resolves the daemon socket: explicit value, then
// $LOOP_DAEMON_SOCK, then the default.
func SocketPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("LOOP_DAEMON_SOCK"); p != "" {
		return p
	}
	p, err := DefaultSocket()
	if err != nil {
		return ""
	}
	return p
}

// Ping is the daemon's liveness answer.
type Ping struct {
	Version       string `json:"version"`
	UptimeSeconds int    `json:"uptime_seconds"`
	Active        int    `json:"active"`
}

// Ping asks the daemon who it is.
func (c *Client) Ping() (Ping, error) {
	var p Ping
	err := c.get("/api/ping", &p)
	return p, err
}

// Dial connects to the daemon at socket and verifies it answers.
func Dial(socket string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socket)
		},
	}
	c := &Client{http: &http.Client{Timeout: 10 * time.Second, Transport: tr}, base: "http://daemon"}
	if _, err := c.Ping(); err != nil {
		return nil, fmt.Errorf("no loop daemon at %s — start one with `loop serve`", socket)
	}
	return c, nil
}

func (c *Client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) get(path string, out any) error { return c.do("GET", path, nil, out) }

// SubmitResult is the answer to a run submission.
type SubmitResult struct {
	RunID       string `json:"run_id"`
	EventsSoFar int    `json:"events_so_far"`
	Warning     string `json:"warning"`
}

// Submit asks the daemon to run (or resume) a pipeline file.
func (c *Client) Submit(file, resumeID string, vars []string) (SubmitResult, error) {
	var res SubmitResult
	err := c.do("POST", "/api/runs", submitRequest{File: file, ResumeID: resumeID, Vars: vars}, &res)
	return res, err
}

// RunInfo mirrors the daemon's view of one run.
type RunInfo struct {
	RunID    string `json:"run_id"`
	Waiting  bool   `json:"waiting"`
	Prompt   string `json:"prompt"`
	Alive    bool   `json:"alive"`
	LastLine string `json:"last_line"`
}

// Run fetches one run's state. The second return is false for unknown ids.
func (c *Client) Run(id string) (RunInfo, bool, error) {
	var info RunInfo
	req, err := http.NewRequest("GET", c.base+"/api/runs/"+id, nil)
	if err != nil {
		return info, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return info, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return info, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return info, false, fmt.Errorf("HTTP %d from /api/runs/%s", resp.StatusCode, id)
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, false, err
	}
	return info, true, nil
}

// Answer posts a gate answer to a waiting run.
func (c *Client) Answer(id, text string) error {
	return c.do("POST", "/api/runs/"+id+"/answer", map[string]string{"text": text}, nil)
}

// Halt pauses a run (SIGINT to the child — resumable).
func (c *Client) Halt(id string) error {
	return c.do("POST", "/api/runs/"+id+"/halt", map[string]string{}, nil)
}

// Event is one streamed run event.
type Event struct {
	Seq   int             `json:"seq"`
	Type  string          `json:"type"`
	Event json.RawMessage `json:"event"`
}

// Events polls a run's event log after the given sequence number — the
// non-streaming counterpart to Stream (UI pages that prefer request/
// response, and tests).
func (c *Client) Events(id string, after int) ([]EventLine, error) {
	var res struct {
		Events []EventLine `json:"events"`
	}
	err := c.do("GET", fmt.Sprintf("/api/runs/%s/events?after=%d", id, after), nil, &res)
	return res.Events, err
}

// Stream subscribes to a run's events from after the given sequence
// number (SSE). The channel closes when the daemon ends the stream (the
// run reached a terminal state and the log went quiet) or ctx is done.
func (c *Client) Stream(ctx context.Context, id string, after int) (<-chan Event, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/runs/%s/stream?after=%d", c.base, id, after), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from stream", resp.StatusCode)
	}
	ch := make(chan Event, 16)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue // event: markers and blank lines
			}
			var ev Event
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
				ch <- ev
			}
		}
	}()
	return ch, nil
}
