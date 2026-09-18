package knowledge

import (
	"os"
	"path/filepath"
	"strings"
)

// Dir is where the knowledge layer lives inside a workspace.
func Dir(ws string) string { return filepath.Join(ws, ".loop", "knowledge") }

func indexPath(ws string) string  { return filepath.Join(Dir(ws), "index.json") }
func digestPath(ws string) string { return filepath.Join(Dir(ws), "digest.md") }
func notePath(ws, slug string) string {
	return filepath.Join(Dir(ws), "notes", slug+".md")
}

// Load reads index.json; a missing or unreadable index is a fresh start
// (nil, nil) — the knowledge layer rebuilds rather than fails.
func Load(ws string) (*Index, error) {
	data, err := os.ReadFile(indexPath(ws))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := unmarshalStrict(data, &idx); err != nil {
		return nil, nil // corrupt index: rebuild, don't block sessions on it
	}
	return &idx, nil
}

// Save writes index.json atomically (temp + rename): it is the layer's
// only state carrier, and a half-written one would read as corruption.
func Save(ws string, idx *Index) error {
	if err := os.MkdirAll(Dir(ws), 0o755); err != nil {
		return err
	}
	data, err := marshal(idx)
	if err != nil {
		return err
	}
	tmp := indexPath(ws) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, indexPath(ws))
}

// WriteNote persists one note body with a provenance header. The header
// is for humans browsing .loop/knowledge/; readers strip it.
func WriteNote(ws string, n Note, body string) error {
	if err := os.MkdirAll(filepath.Dir(notePath(ws, n.Slug)), 0o755); err != nil {
		return err
	}
	body = clipBytes(body, MaxNoteBytes, "… [note clipped]")
	header := "<!-- loop area note · " + n.Slug + " · updated " +
		n.Updated.UTC().Format(timeStamp) + " · coverage: " + coverageLine(n) +
		" · auto-maintained, verify against code -->\n"
	return os.WriteFile(notePath(ws, n.Slug), []byte(header+body), 0o644)
}

// WriteDigest persists digest.md, capped at MaxDigestBytes — the digest
// must stay the small always-on layer it is.
func WriteDigest(ws string, info DigestInfo, body string) error {
	body = clipBytes(body, MaxDigestBytes, "… [digest clipped]")
	header := "<!-- loop project digest · updated " + info.Updated.UTC().Format(timeStamp) +
		" · auto-maintained, verify against code -->\n"
	return os.WriteFile(digestPath(ws), []byte(header+body), 0o644)
}

// stripProvenance drops the leading HTML-comment header line writers add.
func stripProvenance(data []byte) string {
	s := string(data)
	if strings.HasPrefix(s, "<!--") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return strings.TrimSpace(s)
}

// ReadDigest returns digest.md's body; ok is false when absent or empty.
func ReadDigest(ws string) (body string, ok bool) {
	data, err := os.ReadFile(digestPath(ws))
	if err != nil {
		return "", false
	}
	body = stripProvenance(data)
	return body, body != ""
}

// ReadNote returns one note's body; ok is false when the slug is unknown.
func ReadNote(ws, slug string) (body string, ok bool) {
	if !validSlug(slug) {
		return "", false
	}
	data, err := os.ReadFile(notePath(ws, slug))
	if err != nil {
		return "", false
	}
	body = stripProvenance(data)
	return body, body != ""
}

// DigestBlock renders digest.md as the always-on system-prompt block
// room agents carry; empty when no digest exists yet. Re-read per reply,
// so a mid-session auto-refresh is seen by the very next turn.
func DigestBlock(ws string) string {
	body, ok := ReadDigest(ws)
	if !ok {
		return ""
	}
	return "Project knowledge — the maintained digest of this codebase (auto-refreshed; " +
		"it summarizes, so verify specifics against the code):\n" + body
}
