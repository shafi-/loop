package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/engine"
)

func (s *Server) roomJSON(rs *roomSession) roomJSON {
	out := roomJSON{
		Name:            rs.cfg.Name,
		Path:            rs.roomPath,
		Runnable:        rs.runnablePipelines(),
		Commands:        rs.room.CommandList(),
		Busy:            rs.busyNow(),
		PendingApproval: rs.pendingToolAsk(),
		Activity:        rs.activityNow(),
		Roles:           map[string]string{},
	}
	for _, a := range rs.cfg.Agents {
		out.Agents = append(out.Agents, a.Name)
		if a.Role != "" {
			out.Roles[a.Name] = a.Role
		}
	}
	for _, p := range rs.cfg.Pipelines {
		out.Pipelines = append(out.Pipelines, p.Name)
	}
	out.Lines = countLines(filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl"))
	if rs.room.Usage != nil {
		t := rs.room.Usage.Totals()
		if t.Calls > 0 {
			out.Usage = &t
		}
	}
	if all := rs.room.UsageAll(); all.Calls > 0 {
		out.UsageAll = &all
	}
	for _, ru := range rs.sup.Runs() {
		out.Runs = append(out.Runs, s.toRunJSON(ru))
	}
	return out
}

func (s *Server) handleRoomHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.File == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a room \"file\" path")
		return
	}
	rs, err := s.rooms.host(r.Context(), req.File)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.roomJSON(rs))
}

func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	rooms := s.rooms.all()
	out := make([]roomJSON, 0, len(rooms))
	for _, rs := range rooms {
		out = append(out, s.roomJSON(rs))
	}
	// Hosted rooms live in a map: list them by name so clients polling
	// (the dashboard re-renders every few seconds) see a stable order
	// instead of map iteration shuffling.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) roomOf(w http.ResponseWriter, r *http.Request) *roomSession {
	rs, ok := s.rooms.get(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "no hosted room %q", r.PathValue("name"))
		return nil
	}
	return rs
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	if rs := s.roomOf(w, r); rs != nil {
		writeJSON(w, http.StatusOK, s.roomJSON(rs))
	}
}

// handleRoomRuns lists every pipeline run associated with the room —
// active runs first, then runs known from artifacts via transcript
// mentions, newest first. This is the room sidecar's data source.
func (s *Server) handleRoomRuns(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	out := []runJSON{}
	seen := map[string]bool{}
	for _, ru := range rs.sup.Runs() {
		out = append(out, s.toRunJSON(ru))
		seen[ru.RunID] = true
	}
	hist := []runJSON{}
	for _, ref := range rs.mentionedRunIDs() {
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		if rj, ok := s.artifactRun(ref.ID); ok {
			if rj.Alias == "" {
				rj.Alias = ref.Alias
			}
			hist = append(hist, rj)
		}
	}
	sort.Slice(hist, func(i, j int) bool { return hist[i].RunID > hist[j].RunID })
	writeJSON(w, http.StatusOK, map[string]any{"runs": append(out, hist...)})
}

// artifactRun builds a run entry from on-disk artifacts for a run that
// is no longer (or never was) supervised here. Listable only when its
// state recorded a stop point.
func (s *Server) artifactRun(id string) (runJSON, bool) {
	st, err := engine.LoadState(s.runsDir, id)
	if err != nil || st == nil {
		return runJSON{}, false
	}
	out := runJSON{RunID: id, Alive: false}
	switch {
	case st.Done:
		out.Phase = "done"
	case st.Failed != "":
		out.Phase = "failed"
	case st.Paused != "":
		out.Phase = "paused"
	default:
		return runJSON{}, false
	}
	if name := snapshotName(filepath.Join(s.runsDir, id, "pipeline.yaml")); name != "" {
		out.Pipeline = name
	}
	s.mu.Lock()
	out.File = s.specs[id]
	s.mu.Unlock()
	return out, true
}

func (s *Server) handleRoomTranscript(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	after := queryInt(r, "after")
	lines, err := readTranscript(filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl"), after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading transcript: %v", err)
		return
	}
	if lines == nil {
		lines = []TranscriptLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleRoomStream pushes transcript appends (SSE) — the room's live
// feed for every attached client. Rooms never end; the stream closes
// when the client goes away.
func (s *Server) handleRoomStream(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	path := filepath.Join(".loop", "rooms", rs.cfg.Name, "transcript.jsonl")
	after := queryInt(r, "after")
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	// Activity rides the same stream as a named event: the ticker is
	// idle exactly while a persona is busy, which is when the indicator
	// matters. Announce only what persists (activityAfter); clears are
	// immediate. lastKey is this client's last-sent state, so each
	// viewer gets its own transitions; the empty snapshot on connect
	// clears a pill left over from a previous daemon's turn.
	var lastKey string
	emitActivity := func(force bool) {
		act, _ := rs.room.Activity()
		key, payload, send := activityEmission(lastKey, force, act, time.Now(), activityAfter)
		if !send {
			return
		}
		lastKey = key
		fmt.Fprint(w, payload)
		fl.Flush()
	}
	emitActivity(true) // connect snapshot (possibly empty): a pill from a dead turn clears here
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			emitActivity(false)
			lines, err := readTranscript(path, after)
			if err != nil {
				return
			}
			for _, ln := range lines {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"from\":%q,\"text\":%s}\n\n", ln.Seq, ln.From, mustJSON(ln.Text))
				after = ln.Seq
			}
			if len(lines) > 0 {
				fl.Flush()
			}
		}
	}
}

// activityAfter is how long an activity must persist before the room
// announces it — "show only what persists". Faster fragments (a
// millisecond read, a short decision call) would strobe the indicator.
// Clears are never held.
var activityAfter = 2 * time.Second

// activityEmission decides the next SSE activity emission for one
// stream client. Sets are held for hold (an activity still forming
// keeps the previous pill — no clear/show blink on every phase
// transition); clears are immediate; force (the connect snapshot)
// always sends, so a pill left over from a previous daemon's turn
// clears on reconnect. key is the client's new last-sent state
// ("" = idle).
func activityEmission(last string, force bool, act chat.Activity, now time.Time, hold time.Duration) (key, payload string, send bool) {
	if act.What == "" {
		if !force && last == "" {
			return last, "", false
		}
		return "", activityPayload("", ""), true
	}
	key = act.Agent + "\x00" + act.What
	if !force && (key == last || now.Sub(act.Since) < hold) {
		return last, "", false // unchanged, or still forming — keep what is shown
	}
	return key, activityPayload(act.Agent, act.What), true
}

// activityPayload renders one SSE activity frame. Self-identifying
// ("kind":"activity"): transcript-line clients filter on it, since a
// bare data line would decode into a blank RoomLine.
func activityPayload(agent, what string) string {
	return fmt.Sprintf("event: activity\ndata: {\"kind\":\"activity\",\"agent\":%q,\"what\":%s}\n\n", agent, mustJSON(what))
}

// mustJSON renders v as a compact JSON value for embedding in an SSE
// data line; text is the only payload and never fails to encode.
func mustJSON(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (s *Server) handleRoomSay(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"text\" message")
		return
	}
	if err := rs.say(s.serveCtx, req.Text); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) handleRoomRun(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias    string   `json:"alias"`
		ResumeID string   `json:"resume_id,omitempty"`
		Vars     []string `json:"vars,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Alias == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a pipeline \"alias\"")
		return
	}
	id, handoff, err := rs.runWithRoomContext(req.Alias, req.ResumeID, req.Vars)
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.mu.Lock()
	s.specs[id] = req.Alias // room runs keep their alias as the room-side name
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"run_id": id, "alias": req.Alias, "context": handoff})
}

func (s *Server) handleRoomApprove(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias string `json:"alias,omitempty"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"text\" answer")
		return
	}
	// A held shell command from a room agent gets the answer first —
	// it is parked mid-turn on exactly this question. Pipeline gates
	// come after.
	if rs.deliverToolAnswer(req.Text) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := rs.sup.Approve(req.Alias, req.Text); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	// The decision belongs to the room's record: the transcript shows
	// the gate's question, this line shows what the user decided — the
	// chat renders it as the answer chip under the gate line, and the
	// agents read the decision in context on their next turn.
	alias := req.Alias
	if alias == "" {
		for _, run := range rs.sup.Runs() {
			if run.Waiting {
				alias = run.Alias
				break
			}
		}
	}
	_ = rs.tr.Append("user", fmt.Sprintf("approve %s: %s", alias, strings.TrimSpace(req.Text)))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRoomHalt(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Alias string `json:"alias,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := rs.sup.Halt(req.Alias); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRoomReset archives a room's whole conversation and starts a
// fresh one. The archive name comes back so clients can point at it.
func (s *Server) handleRoomReset(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	archive, err := rs.resetRoom()
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"archived": archive})
}

// handleRoomFork keeps the first `through` transcript lines, inclusive,
// and archives the rest — the room continues from that point.
func (s *Server) handleRoomFork(w http.ResponseWriter, r *http.Request) {
	rs := s.roomOf(w, r)
	if rs == nil {
		return
	}
	var req struct {
		Through int `json:"through"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Through < 0 {
		writeError(w, http.StatusBadRequest, "body must be JSON with a non-negative \"through\" line number")
		return
	}
	archive, err := rs.forkRoom(req.Through)
	if err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"archived": archive})
}
