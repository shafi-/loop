package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shafi-/loop/internal/workspace"
)

const timeStamp = "2006-01-02T15:04:05Z"

// Note states after a scan.
const (
	StateFresh   = "fresh"
	StateStale   = "stale" // covered files changed under a written note
	StateMissing = "missing"
)

// Report is the scan's verdict: what a maintenance run would have to do.
type Report struct {
	HadIndex     bool
	LayoutChange bool // directory structure differs from the recorded signature
	DigestState  string
	Notes        map[string]string // slug → state
	Pending      int               // notes needing regeneration
	Fresh        bool              // nothing to do — zero model calls
}

// hashFile is sha256 hex of a file's bytes; "" when unreadable (a hash
// can't distinguish "empty" from "gone", and for staleness both mean
// the note needs a second look).
func hashFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashOf hashes in-memory bytes (for the layout signature).
func hashOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// Inventory is the deterministic layout listing: relative directories
// with their source-file counts, noise skipped, capped. It feeds the
// plan prompt (what areas exist?) and doubles as the layout signature
// (new or removed directories → re-plan).
func Inventory(ws string) string {
	var dirs []string
	counts := map[string]int{}
	root := "."
	filepath.WalkDir(ws, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries just don't exist for us
		}
		rel, rerr := filepath.Rel(ws, path)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		name := d.Name()
		if workspace.IsNoise(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if depth >= 3 {
				return fs.SkipDir
			}
			dirs = append(dirs, rel)
			return nil
		}
		if depth == 0 {
			return nil // root files belong to no inventory dir
		}
		dir := filepath.Dir(rel)
		if dir == "." {
			return nil
		}
		counts[dir]++
		return nil
	})
	_ = root
	sort.Strings(dirs)
	var b strings.Builder
	for i, dir := range dirs {
		if i >= MaxInventoryEntries {
			b.WriteString("… (further directories omitted)\n")
			break
		}
		fmt.Fprintf(&b, "%s/ (%d files)\n", dir, counts[dir])
	}
	return strings.TrimSpace(b.String())
}

// BriefFacts is the small deterministic context every knowledge prompt
// gets: the same facts the workspace brief quotes.
func BriefFacts(ws string) string {
	return workspace.Brief(ws)
}

// KeyHashes records what the digest was built from: the brief's marker
// files plus the layout signature. Returned sorted by key for stable JSON.
func KeyHashes(ws string) map[string]string {
	out := map[string]string{}
	for _, f := range workspace.Markers(ws) {
		if h := hashFile(filepath.Join(ws, f)); h != "" {
			out[f] = h
		}
	}
	if inv := Inventory(ws); inv != "" {
		out[layoutHashKey] = hashOf(inv)
	}
	return out
}

// covers reports whether rel (workspace-relative, slash-separated) is
// inside the note's coverage.
func covers(n Note, rel string) bool {
	for _, f := range n.Files {
		if rel == f {
			return true
		}
	}
	for _, dir := range n.Dirs {
		dir = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(dir)), "/")
		if dir == "" || dir == "." {
			continue
		}
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// noteFiles lists the note's covered files that exist, sorted.
func noteFiles(ws string, n Note) []string {
	var out []string
	for _, dir := range n.Dirs {
		clean := filepath.Clean(dir)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		root := filepath.Join(ws, clean)
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if workspace.IsNoise(d.Name()) && path != root {
					return fs.SkipDir
				}
				return nil
			}
			if workspace.IsNoise(d.Name()) {
				return nil
			}
			rel, rerr := filepath.Rel(ws, path)
			if rerr == nil {
				out = append(out, rel)
			}
			return nil
		})
	}
	for _, f := range n.Files {
		clean := filepath.Clean(f)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			continue
		}
		if _, err := os.Stat(filepath.Join(ws, clean)); err == nil {
			out = append(out, filepath.ToSlash(clean))
		}
	}
	sort.Strings(out)
	out = dedup(out)
	return out
}

func dedup(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// coverageLine renders a note's coverage as one compact string.
func coverageLine(n Note) string {
	var parts []string
	for _, d := range n.Dirs {
		parts = append(parts, filepath.ToSlash(d)+"/")
	}
	parts = append(parts, n.Files...)
	return strings.Join(parts, " ")
}

// Scan rehashes the index's coverage against the workspace: pure file
// I/O, zero tokens. A missing index or digest means nothing is fresh.
func Scan(ws string, idx *Index) Report {
	rep := Report{Notes: map[string]string{}}
	if idx == nil {
		rep.DigestState = StateMissing
		rep.Fresh = false
		return rep
	}
	rep.HadIndex = true

	// Digest staleness: any recorded key hash that drifted.
	current := KeyHashes(ws)
	rep.DigestState = digestState(idx.Digest, current, ws)

	// Layout drift means the area partition itself may be wrong.
	if idx.Digest.Hashes[layoutHashKey] != "" && current[layoutHashKey] != idx.Digest.Hashes[layoutHashKey] {
		rep.LayoutChange = true
	}

	for _, n := range idx.Notes {
		state := StateFresh
		if _, err := os.Stat(notePath(ws, n.Slug)); err != nil {
			state = StateMissing
		} else {
			for _, rel := range noteFiles(ws, n) {
				want := n.Hashes[rel]
				if want == "" || hashFile(filepath.Join(ws, rel)) != want {
					state = StateStale
					break
				}
			}
			// A note whose coverage has no files at all is stale: the
			// code it described is gone.
			if state == StateFresh && len(noteFiles(ws, n)) == 0 {
				state = StateStale
			}
		}
		rep.Notes[n.Slug] = state
		if state != StateFresh {
			rep.Pending++
		}
	}
	rep.Fresh = rep.DigestState == StateFresh && rep.Pending == 0
	return rep
}

func digestState(d DigestInfo, current map[string]string, ws string) string {
	if _, err := os.Stat(digestPath(ws)); err != nil {
		return StateMissing
	}
	if len(d.Hashes) == 0 {
		return StateStale
	}
	for k, want := range d.Hashes {
		if current[k] != want {
			return StateStale
		}
	}
	return StateFresh
}

func marshal(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func unmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	return dec.Decode(v)
}

func clipBytes(s string, limit int, marker string) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + marker
}

// validSlug guards filenames built from model-provided area names.
func validSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
