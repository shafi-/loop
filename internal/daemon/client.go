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

	"github.com/shafi-/loop/internal/usage"
)

// Client speaks to a loop daemon over its unix socket. Shared by the
// CLI's --daemon attach mode and (next) the web UI, so every surface
// drives runs through the same API.
type Client struct {
	http *http.Client
	slow *http.Client // long-haul calls (drafting) beyond the default timeout
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

// PortSocket is the daemon socket for a `serve --port N` daemon:
// derived from the port so two project daemons coexist without anyone
// typing a socket path. Plumbing — the port is the only user-facing
// identity; this file is an implementation detail.
func PortSocket(port int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loop", fmt.Sprintf("daemon-%d.sock", port)), nil
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
	// Workspace names the directory the daemon was started from — the
	// web UI's header shows it so two project tabs are tellable apart.
	Workspace string `json:"workspace,omitempty"`
}

// Ping asks the daemon who it is.
func (c *Client) Ping() (Ping, error) {
	var p Ping
	err := c.get("/api/ping", &p)
	return p, err
}

// Workspace is the daemon's answer for pickers: candidate files from
// the workspace's conventional directories (see WorkspaceFile).
type Workspace struct {
	Workspace string          `json:"workspace"`
	Pipelines []WorkspaceFile `json:"pipelines"`
	Rooms     []WorkspaceFile `json:"rooms"`
}

// Workspace asks the daemon which pipeline and room files its
// workspace offers.
func (c *Client) Workspace() (Workspace, error) {
	var wk Workspace
	err := c.get("/api/workspace", &wk)
	return wk, err
}

// Dial connects to the daemon at socket and verifies it answers.
func Dial(socket string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socket)
		},
	}
	c := &Client{
		http: &http.Client{Timeout: 10 * time.Second, Transport: tr},
		slow: &http.Client{Timeout: 3 * time.Minute, Transport: tr},
		base: "http://daemon",
	}
	if _, err := c.Ping(); err != nil {
		return nil, fmt.Errorf("no loop daemon at %s — start one with `loop serve`", socket)
	}
	return c, nil
}

func (c *Client) do(method, path string, body, out any) error {
	return c.call(c.http, method, path, body, out)
}

// doLong is do, on the patient client: drafting waits on a model, not
// on the daemon.
func (c *Client) doLong(method, path string, body, out any) error {
	return c.call(c.slow, method, path, body, out)
}

func (c *Client) call(hc *http.Client, method, path string, body, out any) error {
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
	resp, err := hc.Do(req)
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
	Alias    string `json:"alias"` // the room-side name for room runs
	Pipeline string `json:"pipeline"`
	Phase    string `json:"phase"` // running | waiting | done | failed | paused
	File     string `json:"file"`  // set when this daemon submitted the run
	Waiting  bool   `json:"waiting"`
	Prompt   string `json:"prompt"`
	Alive    bool   `json:"alive"`
	LastLine string `json:"last_line"`
}

// Runs lists active runs; with history, also runs known from artifacts
// (finished elsewhere or in past daemon lives), newest first.
func (c *Client) Runs(history bool) ([]RunInfo, error) {
	path := "/api/runs"
	if history {
		path += "?history=1"
	}
	var res struct {
		Runs []RunInfo `json:"runs"`
	}
	err := c.do("GET", path, nil, &res)
	return res.Runs, err
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

// ─── rooms ───────────────────────────────────────────────────────────

// RoomInfo describes a hosted room.
type RoomInfo struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Agents    []string          `json:"agents"`
	Roles     map[string]string `json:"roles"` // agent name -> role, when set
	Pipelines []string          `json:"pipelines"`
	Busy      bool              `json:"busy"`
	Lines     int               `json:"transcript_lines"`
	Runs      []RunInfo         `json:"runs"`  // the room's own pipeline runs
	Usage     *usage.Total      `json:"usage"` // the session's token ledger, when calls were made
}

// HostRoom hosts (or attaches to) a room session on the daemon.
func (c *Client) HostRoom(file string) (RoomInfo, error) {
	var res RoomInfo
	err := c.do("POST", "/api/rooms", map[string]string{"file": file}, &res)
	return res, err
}

// Rooms lists hosted rooms.
func (c *Client) Rooms() ([]RoomInfo, error) {
	var res struct {
		Rooms []RoomInfo `json:"rooms"`
	}
	err := c.get("/api/rooms", &res)
	return res.Rooms, err
}

// Room fetches one hosted room; false when the name is not hosted.
func (c *Client) Room(name string) (RoomInfo, bool, error) {
	var res RoomInfo
	req, err := http.NewRequest("GET", c.base+"/api/rooms/"+name, nil)
	if err != nil {
		return res, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return res, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return res, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, false, fmt.Errorf("HTTP %d from /api/rooms/%s", resp.StatusCode, name)
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return res, false, err
	}
	return res, true, nil
}

// RoomLine is one transcript line from a room stream or poll. At is the
// message's recorded timestamp (zero for legacy lines without one).
type RoomLine struct {
	Seq  int       `json:"seq"`
	From string    `json:"from"`
	Text string    `json:"text"`
	At   time.Time `json:"at,omitempty"`
}

// RoomTranscript polls a room's transcript after the given line number.
func (c *Client) RoomTranscript(name string, after int) ([]RoomLine, error) {
	var res struct {
		Lines []RoomLine `json:"lines"`
	}
	err := c.do("GET", fmt.Sprintf("/api/rooms/%s/transcript?after=%d", name, after), nil, &res)
	return res.Lines, err
}

// RoomStream subscribes to a room's transcript appends (SSE). The
// channel closes when the connection drops or ctx is done — rooms never
// end, so there is no server-side "end" event.
func (c *Client) RoomStream(ctx context.Context, name string, after int) (<-chan RoomLine, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/rooms/%s/stream?after=%d", c.base, name, after), nil)
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
		return nil, fmt.Errorf("HTTP %d from room stream", resp.StatusCode)
	}
	ch := make(chan RoomLine, 16)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ln RoomLine
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ln) == nil {
				ch <- ln
			}
		}
	}()
	return ch, nil
}

// RoomRuns lists every pipeline run associated with a hosted room —
// active runs first, then runs known from artifacts, newest first.
// This feeds the room page's sidecar.
func (c *Client) RoomRuns(name string) ([]RunInfo, error) {
	var res struct {
		Runs []RunInfo `json:"runs"`
	}
	err := c.do("GET", "/api/rooms/"+name+"/runs", nil, &res)
	return res.Runs, err
}

// Say posts one user message to a hosted room; the turn is processed in
// the background and shows up as transcript appends.
func (c *Client) Say(name, text string) error {
	return c.do("POST", "/api/rooms/"+name+"/say", map[string]string{"text": text}, nil)
}

// RoomRun starts one of the room's owned pipelines in the room context.
func (c *Client) RoomRun(name, alias, resumeID string, vars []string) (string, error) {
	var res struct {
		RunID string `json:"run_id"`
	}
	body := map[string]any{"alias": alias}
	if resumeID != "" {
		body["resume_id"] = resumeID
	}
	if len(vars) > 0 {
		body["vars"] = vars
	}
	err := c.do("POST", "/api/rooms/"+name+"/run", body, &res)
	return res.RunID, err
}

// RoomApprove answers a waiting gate in the room (alias optional when
// exactly one gate is asking).
func (c *Client) RoomApprove(name, alias, text string) error {
	return c.do("POST", "/api/rooms/"+name+"/approve", map[string]string{"alias": alias, "text": text}, nil)
}

// RoomHalt pauses a room run (alias optional when one run is active).
func (c *Client) RoomHalt(name, alias string) error {
	return c.do("POST", "/api/rooms/"+name+"/halt", map[string]string{"alias": alias}, nil)
}

// RoomReset archives a room's whole conversation and starts a fresh
// one; the archive file's name comes back.
func (c *Client) RoomReset(name string) (string, error) {
	var res struct {
		Archived string `json:"archived"`
	}
	err := c.do("POST", "/api/rooms/"+name+"/reset", nil, &res)
	return res.Archived, err
}

// RoomFork keeps the room's first through transcript lines, inclusive,
// and archives everything after them.
func (c *Client) RoomFork(name string, through int) (string, error) {
	var res struct {
		Archived string `json:"archived"`
	}
	err := c.do("POST", "/api/rooms/"+name+"/fork", map[string]int{"through": through}, &res)
	return res.Archived, err
}

// ─── authoring: draft, validate, save ────────────────────────────────

// ValidationIssue is one problem with a document: where, and what.
type ValidationIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationResult is the answer to a validate call; Errors is nil
// when OK.
type ValidationResult struct {
	OK     bool              `json:"ok"`
	Errors []ValidationIssue `json:"errors"`
}

// Draft asks the daemon to turn a plain-language description into
// drafted YAML (kind: "pipeline" or "room"). The generator's honesty
// contract holds: a failure returns as an error, never a guess.
func (c *Client) Draft(kind, description string) (string, error) {
	var res struct {
		YAML string `json:"yaml"`
	}
	err := c.doLong("POST", "/api/workspace/draft",
		map[string]string{"kind": kind, "description": description}, &res)
	return res.YAML, err
}

// Validate checks YAML with the daemon's strict parser — the same
// report `loop validate` would give the saved file.
func (c *Client) Validate(kind, content string) (ValidationResult, error) {
	var res ValidationResult
	err := c.do("POST", "/api/workspace/validate",
		map[string]string{"kind": kind, "content": content}, &res)
	return res, err
}

// Save validates and writes the document into the kind's workspace
// directory; the saved path comes back. When the target exists, the
// daemon refuses (409) — retry with overwrite confirmed by the user.
func (c *Client) Save(kind, content string, overwrite bool) (string, error) {
	var res struct {
		Path string `json:"path"`
	}
	err := c.do("POST", "/api/workspace/save",
		map[string]any{"kind": kind, "content": content, "overwrite": overwrite}, &res)
	return res.Path, err
}

// ─── personas ────────────────────────────────────────────────────────

// PersonaInfo is one listed persona and where it lives.
type PersonaInfo struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Scope string `json:"scope"` // "project" | "global"
	Path  string `json:"path"`
}

// Personas lists both libraries: project first, then global.
func (c *Client) Personas() ([]PersonaInfo, error) {
	var res struct {
		Personas []PersonaInfo `json:"personas"`
	}
	err := c.get("/api/personas", &res)
	return res.Personas, err
}

// PersonaDetail returns a persona's YAML text for editing.
func (c *Client) PersonaDetail(scope, name string) (string, error) {
	var res struct {
		YAML string `json:"yaml"`
	}
	err := c.do("GET", "/api/personas/detail?scope="+scope+"&name="+name, nil, &res)
	return res.YAML, err
}

// SavePersona saves a persona into one of the libraries ("project" or
// "global"); same validate-then-write, overwrite-refuses contract as
// Save.
func (c *Client) SavePersona(scope, content string, overwrite bool) (string, error) {
	var res struct {
		Path string `json:"path"`
	}
	err := c.do("POST", "/api/workspace/save",
		map[string]any{"kind": "persona", "scope": scope, "content": content, "overwrite": overwrite}, &res)
	return res.Path, err
}

// DeletePersona removes a persona from one library. The daemon refuses
// while workspace documents still reference it.
func (c *Client) DeletePersona(scope, name string) (string, error) {
	var res struct {
		Removed string `json:"removed"`
	}
	err := c.do("POST", "/api/personas/delete",
		map[string]string{"scope": scope, "name": name}, &res)
	return res.Removed, err
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
