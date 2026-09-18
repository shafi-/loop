// Package knowledge is loop's layered project memory: a maintained
// digest and per-area notes under .loop/knowledge/ so room agents can
// orient from a summary instead of re-reading the codebase every
// session. Storage is deterministic files; models only summarize.
// Maintenance is incremental — a hash scan costs zero tokens when
// nothing changed — and every model call is metered under the
// "knowledge" label.
package knowledge

import "time"

// Caps keep each layer a context, not a dump: the digest rides every
// agent's system prompt, notes are pulled one at a time.
const (
	// MaxRefreshCalls bounds one maintenance run; the rest is deferred
	// to the next one (announced, never silent).
	MaxRefreshCalls = 8
	// MaxAreas caps the area partition of a workspace. Deliberately above
	// MaxRefreshCalls: seeding a larger repo is incremental by design —
	// a first run fills the budget and defers the rest to the next one.
	MaxAreas = 12
	// MaxAreaInput bounds one note's summarize input (files of one area).
	MaxAreaInput = 32 << 10
	// MaxDigestInput bounds the digest merge input (the notes).
	MaxDigestInput = 24 << 10
	// PerFileClip bounds any single file inside a summarize input.
	PerFileClip = 12 << 10
	// MaxDigestBytes caps digest.md at write and at prompt time — the
	// digest must stay the small always-on layer it replaced nothing for.
	MaxDigestBytes = 3 << 10
	// MaxNoteBytes caps a note body at write time (700 output tokens ≈ 3 KiB).
	MaxNoteBytes = 4 << 10
	// MaxInventoryEntries caps the layout listing a plan call sees.
	MaxInventoryEntries = 60

	maxNoteOutputTokens   = 700
	maxDigestOutputTokens = 600

	// UsageLabel is the ledger label for every knowledge model call —
	// /cost reports it alongside per-agent numbers.
	UsageLabel = "knowledge"

	layoutHashKey = ".layout" // pseudo-entry in DigestInfo.Hashes
)

// Note is one area's maintained summary and what it covers. Coverage is
// workspace-relative: Dirs entries are directory prefixes ("internal/chat"),
// Files entries are exact files.
type Note struct {
	Slug    string            `json:"slug"`
	Title   string            `json:"title"`
	Scope   string            `json:"scope"` // one line: what this area is
	Dirs    []string          `json:"dirs"`
	Files   []string          `json:"files"`
	Hashes  map[string]string `json:"hashes,omitempty"` // covered file → sha256 at generation
	Updated time.Time         `json:"updated_at,omitempty"`
}

// DigestInfo records what digest.md was built from, so a scan can tell
// stale from fresh: the brief's key files plus a layout signature (new
// or removed directories mean the area partition itself may be wrong).
type DigestInfo struct {
	Hashes  map[string]string `json:"hashes,omitempty"`
	Updated time.Time         `json:"updated_at,omitempty"`
}

// Index is .loop/knowledge/index.json — the knowledge layer's state.
type Index struct {
	Version int        `json:"version"`
	Digest  DigestInfo `json:"digest"`
	Notes   []Note     `json:"notes"`
}
