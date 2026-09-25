package chat

import (
	"time"
)

// Room activity: what the room is doing right now, ephemeral by
// design — it never touches the transcript (the record, the agents'
// context, and the SSE payload), so surfaces that want an indicator
// read it here instead. In-memory is the honest home: activity that
// outlived the daemon would be a lie.
//
// The tracker is written by the turn goroutine only (Say → reply →
// the tool/approval wrappers are one goroutine) and read concurrently
// by hosting surfaces; actMu guards that.

// Activity is one snapshot of the room's current work. What == "" is
// the inactive state (never keyed on Since — a zero time would pass
// any age check).
type Activity struct {
	Agent string // the persona doing it; "" = the room itself
	What  string // "" = nothing happening (the inactive state)
	Since time.Time
}

// setActivity records what the room is doing now.
func (r *Room) setActivity(agent, what string) {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	r.act = Activity{Agent: agent, What: what, Since: time.Now()}
}

// clearActivity marks the room idle.
func (r *Room) clearActivity() {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	r.act = Activity{}
}

// Activity returns what the room is doing right now; ok is false when
// the room is idle. Raw truth — the announce threshold ("show only
// what persists ≥2s") is a presentation rule applied by the surfaces
// reading this, not by the tracker.
func (r *Room) Activity() (Activity, bool) {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	if r.act.What == "" {
		return Activity{}, false
	}
	return r.act, true
}
