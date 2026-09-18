package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// Manager owns one workspace's knowledge layer: it scans, and when
// (and only when) the scan says something drifted, it spends model calls
// to re-summarize. Providers are wrapped under the "knowledge" ledger
// label, so /cost shows exactly what maintenance spent.
type Manager struct {
	// WS is the workspace root the knowledge lives in.
	WS string
	// Provider is the raw provider; the manager wraps it with the meter.
	Provider llm.Provider
	// Model is the model id requests carry.
	Model string
	// Meter records every call under "knowledge" (nil = unmetered).
	Meter *usage.Meter
	// Logf receives progress notices (may be nil).
	Logf func(format string, args ...any)

	mu sync.Mutex // one maintenance at a time; a second is skipped, not queued
}

// Result summarizes one maintenance run.
type Result struct {
	Calls         int
	Updated       []string // note slugs rewritten
	Deferred      []string // skipped by the call cap; next run catches them
	Failed        []string // slug: reason
	DigestUpdated bool
	Message       string // one-line human summary
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// MaintainLine runs a maintenance and renders its one-line transcript
// announcement: "" when nothing needed doing (already fresh, or another
// maintenance is in flight). Errors become the line — a refresh must
// never fail the turn that triggered it.
func (m *Manager) MaintainLine(ctx context.Context) string {
	res, err := m.Maintain(ctx)
	if err != nil {
		return "◈ knowledge refresh failed: " + err.Error()
	}
	if res.Message == "" || res.Message == "knowledge up to date" {
		return ""
	}
	return "◈ knowledge: " + res.Message
}

func (m *Manager) provider() llm.Provider {
	return usage.Wrap(m.Provider, m.Meter, UsageLabel)
}

// Maintain brings the knowledge layer up to date: scan first (zero
// tokens when nothing changed), plan only when the index is missing or
// the layout drifted, summarize only stale/missing notes, merge the
// digest only when something under it moved. Per-note and digest
// failures never abort the run — they land in Result.Failed.
func (m *Manager) Maintain(ctx context.Context) (Result, error) {
	if !m.mu.TryLock() {
		return Result{Message: ""}, nil // a maintenance is already in flight
	}
	defer m.mu.Unlock()

	res := Result{}
	idx, err := Load(m.WS)
	if err != nil {
		return res, err
	}
	rep := Scan(m.WS, idx)
	if rep.Fresh {
		res.Message = "knowledge up to date"
		return res, nil
	}
	// A workspace with no structure — no source directories — has
	// nothing to summarize. The layer stays off rather than burning
	// calls inventing areas for an empty directory.
	if Inventory(m.WS) == "" && idx == nil {
		return Result{}, nil
	}
	p := m.provider()

	// Plan: first seed, or the directory structure moved under an
	// existing partition (new areas can be invisible to hash staleness).
	if idx == nil || rep.LayoutChange {
		var fresh bool
		idx, fresh, err = m.plan(ctx, p, idx)
		if err != nil {
			return res, fmt.Errorf("planning areas: %w", err)
		}
		if fresh {
			res.Calls++
		}
		// A just-planned note is "missing" until written; rescan states
		// for the new entries.
		rep = Scan(m.WS, idx)
	}

	// Summarize stale/missing notes, deterministic order, capped.
	budget := MaxRefreshCalls
	for _, n := range idx.Notes {
		if rep.Notes[n.Slug] == StateFresh {
			continue
		}
		if budget <= 0 {
			res.Deferred = append(res.Deferred, n.Slug)
			continue
		}
		body, err := m.summarize(ctx, p, n)
		if err != nil {
			res.Failed = append(res.Failed, n.Slug+": "+err.Error())
			continue
		}
		n.Hashes = fileHashes(m.WS, noteFiles(m.WS, n))
		n.Updated = time.Now().UTC()
		if err := WriteNote(m.WS, n, body); err != nil {
			res.Failed = append(res.Failed, n.Slug+": "+err.Error())
			continue
		}
		setNote(idx, n)
		res.Updated = append(res.Updated, n.Slug)
		res.Calls++
		budget--
	}

	// Digest: rebuild when it moved or anything under it did.
	if len(res.Updated) > 0 || rep.DigestState != StateFresh {
		if err := m.rebuildDigest(ctx, p, idx, &res); err != nil {
			res.Failed = append(res.Failed, "digest: "+err.Error())
		}
	}
	if err := Save(m.WS, idx); err != nil {
		return res, err
	}
	res.Message = summarize2(res)
	m.logf("◈ knowledge: %s", res.Message)
	return res, nil
}

// NotesReport renders /notes output: the area-note index, or one note's
// body. Deterministic reads — zero model calls.
func NotesReport(ws string, args []string) string {
	if ws == "" {
		return "◈ /notes: this room has no workspace attached"
	}
	if len(args) > 0 {
		slug := args[0]
		body, ok := ReadNote(ws, slug)
		if !ok {
			return "◈ no note " + slug + " — run /notes to list what exists"
		}
		return "◈ note " + slug + " (maintained summary — verify against code):\n" + body
	}
	idx, err := Load(ws)
	if err != nil || idx == nil || len(idx.Notes) == 0 {
		return "◈ no project knowledge yet — it is built after the first implementation turn (or run: loop digest)"
	}
	digestLine := "digest: not built yet"
	if _, ok := ReadDigest(ws); ok {
		digestLine = "digest: present"
	}
	var b strings.Builder
	b.WriteString("◈ project notes — " + digestLine + "\narea notes:\n")
	for _, n := range idx.Notes {
		fmt.Fprintf(&b, "  %s — %s: %s\n", n.Slug, n.Title, n.Scope)
	}
	b.WriteString("read one with /notes <slug>")
	return b.String()
}

func summarize2(res Result) string {
	if len(res.Updated) == 0 && !res.DigestUpdated {
		msg := "knowledge up to date"
		if len(res.Failed) > 0 {
			msg += fmt.Sprintf(" (%d failed — retries next run)", len(res.Failed))
		}
		return msg
	}
	var parts []string
	if len(res.Updated) > 0 {
		parts = append(parts, fmt.Sprintf("updated %d area notes", len(res.Updated)))
	}
	if res.DigestUpdated {
		parts = append(parts, "rebuilt digest")
	}
	msg := strings.Join(parts, " + ") + fmt.Sprintf(" (%d calls)", res.Calls)
	if len(res.Deferred) > 0 {
		msg += fmt.Sprintf("; %d deferred to the next run (call cap)", len(res.Deferred))
	}
	if len(res.Failed) > 0 {
		msg += fmt.Sprintf("; %d failed (retry next run)", len(res.Failed))
	}
	return msg
}

func setNote(idx *Index, n Note) {
	for i := range idx.Notes {
		if idx.Notes[i].Slug == n.Slug {
			idx.Notes[i] = n
			return
		}
	}
	idx.Notes = append(idx.Notes, n)
}

func fileHashes(ws string, files []string) map[string]string {
	out := map[string]string{}
	for _, rel := range files {
		if h := hashFile(filepath.Join(ws, filepath.FromSlash(rel))); h != "" {
			out[rel] = h
		}
	}
	return out
}

// plan asks the model to partition the workspace, merging the proposal
// into the index: existing notes survive, new areas join. Returns the
// index and whether a plan call was spent.
func (m *Manager) plan(ctx context.Context, p llm.Provider, idx *Index) (*Index, bool, error) {
	prompt := "Project inventory (directories with source-file counts):\n" + Inventory(m.WS) +
		"\n\nProject facts:\n" + BriefFacts(m.WS) +
		"\n\nPartition this codebase into areas for a maintained knowledge base."
	resp, err := p.Complete(ctx, llm.Request{
		Model: m.Model,
		System: `You partition a codebase into areas for a maintained knowledge base.
An area is a coherent subsystem — a feature, a layer, a tool — small
enough to summarize from a handful of files. Prefer 3 to 8 areas.
Coverage is workspace-relative directories and files; every meaningful
source directory belongs to exactly one area. Root config files (go.mod,
package.json, README) belong to no area. Names are short (one or two
words); scope is one sentence.`,
		Messages:       []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		ResponseSchema: planSchema,
		MaxTokens:      900,
	})
	if err != nil {
		return idx, false, err
	}
	notes, err := parsePlan(resp.Text, m.WS)
	if err != nil {
		return idx, false, err
	}
	if idx == nil {
		idx = &Index{Version: 1}
	}
	for _, n := range notes {
		if findNote(idx, n.Slug) == nil {
			idx.Notes = append(idx.Notes, n)
		}
	}
	return idx, true, nil
}

func findNote(idx *Index, slug string) *Note {
	for i := range idx.Notes {
		if idx.Notes[i].Slug == slug {
			return &idx.Notes[i]
		}
	}
	return nil
}

// summarize renders one area's note from its covered files.
func (m *Manager) summarize(ctx context.Context, p llm.Provider, n Note) (string, error) {
	files := noteFiles(m.WS, n)
	if len(files) == 0 {
		return "", fmt.Errorf("no files under coverage %s", coverageLine(n))
	}
	input, used := clipFiles(m.WS, files, PerFileClip, MaxAreaInput)
	if used == 0 {
		return "", fmt.Errorf("coverage %s is unreadable", coverageLine(n))
	}
	resp, err := p.Complete(ctx, llm.Request{
		Model: m.Model,
		System: `You write one area-note of a project's knowledge base — a durable
summary so future sessions never re-read these files. Markdown, at most
about 40 lines: what this area does; its key files with a one-line role
each; how it connects to the rest of the project; non-obvious invariants
or gotchas. Facts only. No preamble, no applause.`,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Area: " + n.Title + " — " + n.Scope +
			"\nCoverage: " + coverageLine(n) +
			"\n\nFiles:\n\n" + input}},
		MaxTokens: maxNoteOutputTokens,
	})
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(resp.Text)
	if body == "" {
		return "", fmt.Errorf("empty summary")
	}
	return body, nil
}

// rebuildDigest merges the area notes plus brief facts into digest.md.
// The index records what it was built from — without that, every later
// scan would read the digest as stale.
func (m *Manager) rebuildDigest(ctx context.Context, p llm.Provider, idx *Index, res *Result) error {
	var b strings.Builder
	for _, n := range idx.Notes {
		body, ok := ReadNote(m.WS, n.Slug)
		if !ok {
			continue // not yet written (deferred/failed) — digest builds from what exists
		}
		fmt.Fprintf(&b, "## %s (%s)\n%s\n\n", n.Title, n.Slug, clipBytes(body, 3<<10, "… [clipped]"))
	}
	if b.Len() == 0 {
		return fmt.Errorf("no notes to digest yet")
	}
	prompt := "Project facts:\n" + BriefFacts(m.WS) + "\n\nArea notes:\n\n" +
		clipBytes(b.String(), MaxDigestInput, "\n… [notes clipped]\n")
	resp, err := p.Complete(ctx, llm.Request{
		Model: m.Model,
		System: `You maintain a project digest: the always-on summary every agent in
this workspace sees. Markdown, at most about 30 lines: what the project
is and for whom; how it is structured (one short paragraph); conventions
worth honoring; current state — what was recently built or changed, per
the notes' own "updated" headers. End with an index of the area notes,
one line each: "- <slug> — <scope>". No preamble.`,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		MaxTokens: maxDigestOutputTokens,
	})
	if err != nil {
		return err
	}
	body := strings.TrimSpace(resp.Text)
	if body == "" {
		return fmt.Errorf("empty digest")
	}
	info := DigestInfo{Hashes: KeyHashes(m.WS), Updated: time.Now().UTC()}
	if err := WriteDigest(m.WS, info, body); err != nil {
		return err
	}
	idx.Digest = info
	res.DigestUpdated = true
	res.Calls++
	return nil
}

// clipFiles renders files as --- path --- sections, head-clipped per
// file and total-clipped, deterministically (sorted, walk until spent).
func clipFiles(ws string, files []string, perFile, total int) (string, int) {
	var b strings.Builder
	spent := 0
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > perFile {
			content = content[:perFile] + "\n… [file clipped]"
		}
		section := "--- " + rel + " ---\n" + content + "\n\n"
		if spent+len(section) > total {
			rem := total - spent
			if rem > 64 {
				b.WriteString(section[:rem] + "\n… [area input clipped]\n")
				spent += rem
			}
			break
		}
		b.WriteString(section)
		spent += len(section)
	}
	return b.String(), spent
}
