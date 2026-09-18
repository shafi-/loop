// Package chat orchestrates multi-agent rooms: user messages, forced
// replies from tagged agents, and speak-or-silent decisions from
// observers, with a persistent transcript.
package chat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Message is one transcript entry.
type Message struct {
	TS   time.Time `json:"ts"`
	From string    `json:"from"` // "user" or an agent name
	Text string    `json:"text"`
}

// Transcript is the room's shared memory: append-only on disk, windowed
// in prompts. It is safe for concurrent use — the room loop, pipeline
// status relays, and agent tool hooks append from different goroutines.
type Transcript struct {
	Messages []Message
	path     string // empty = in-memory only (tests)

	mu sync.Mutex
}

// OpenTranscript loads (or creates) the transcript at dir/transcript.jsonl.
func OpenTranscript(dir string) (*Transcript, error) {
	t := &Transcript{path: filepath.Join(dir, "transcript.jsonl")}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(t.path)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
			t.Messages = append(t.Messages, m)
		}
	}
	return t, sc.Err()
}

// Append records a message and persists it immediately.
func (t *Transcript) Append(from, text string) error {
	m := Message{TS: time.Now().UTC(), From: from, Text: text}
	t.mu.Lock()
	t.Messages = append(t.Messages, m)
	t.mu.Unlock()
	if t.path == "" {
		return nil
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Window returns at most n recent messages, oldest first. The result is
// a copy: callers iterate after the lock is released, and Append may
// write into the original's spare capacity concurrently.
func (t *Transcript) Window(n int) []Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	msgs := t.Messages
	if n > 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out
}

// Rotate archives the current transcript as <dir>/<archive> and
// restarts from the first keep messages, recording note as the new
// transcript's last line (the fork point marker, or a reset's opening
// announcement). The in-memory slice is replaced to match, and the
// same *Transcript stays valid — the room's memory is replaced in
// place, the room itself never notices.
func (t *Transcript) Rotate(archive string, keep int, note string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if keep < 0 {
		keep = 0
	}
	if keep > len(t.Messages) {
		keep = len(t.Messages)
	}
	kept := make([]Message, keep)
	copy(kept, t.Messages[:keep])
	t.Messages = append(kept, Message{TS: time.Now().UTC(), From: "system", Text: note})
	if t.path == "" {
		return nil // in-memory only (tests)
	}
	if err := os.Rename(t.path, filepath.Join(filepath.Dir(t.path), archive)); err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	for _, m := range t.Messages {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(t.path, []byte(b.String()), 0o644)
}

// Compact summarizes away history: the last keep messages stay live,
// everything before them is archived to <dir>/<archive> (never
// destroyed), and summary rides as the new first line — the "session
// so far" every subsequent prompt opens with.
func (t *Transcript) Compact(archive string, keep int, summary string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if keep < 0 {
		keep = 0
	}
	if keep > len(t.Messages) {
		keep = len(t.Messages)
	}
	kept := make([]Message, keep)
	copy(kept, t.Messages[len(t.Messages)-keep:])
	t.Messages = append([]Message{{TS: time.Now().UTC(), From: "system", Text: summary}}, kept...)
	if t.path == "" {
		return nil // in-memory only (tests)
	}
	if err := os.Rename(t.path, filepath.Join(filepath.Dir(t.path), archive)); err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	for _, m := range t.Messages {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(t.path, []byte(b.String()), 0o644)
}

// Render formats messages with explicit attribution — the form every
// agent prompt sees ([user] …, [ceo] …).
func Render(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "[%s] %s\n", m.From, m.Text)
	}
	return b.String()
}

// Context budget defaults and clips. The window counts messages; the
// budget counts bytes — a window of 50 huge pastes would otherwise ride
// at full size into every prompt.
const (
	// DefaultMaxContextBytes bounds the conversation string sent to any
	// model (settings.max_context_bytes tunes it per room).
	DefaultMaxContextBytes = 24 << 10
	// PerMessageClip caps any single rendered message; the full text
	// always stays in transcript.jsonl.
	PerMessageClip = 8 << 10
	// DecisionWindowMessages / DecisionContextBytes bound the
	// speak-or-silent call: recency is what it needs, not depth.
	DecisionWindowMessages = 10
	DecisionContextBytes   = 6 << 10

	omittedMarker   = "[earlier messages omitted]"
	clippedSuffix   = "\n… [message clipped]"
)

// BuildConversation renders the windowed transcript as one attributed
// conversation string for prompts, newest-priority under a byte budget:
// messages render from the newest backwards until maxBytes is spent
// (any single message clips at PerMessageClip with a visible marker),
// and dropped history is announced by one leading omittedMarker line.
// Deterministic — same transcript and budget, same string.
func (t *Transcript) BuildConversation(window, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxContextBytes
	}
	msgs := t.Window(window)
	rendered := make([]string, len(msgs))
	for i, m := range msgs {
		text := m.Text
		if len(text) > PerMessageClip {
			text = text[:PerMessageClip] + clippedSuffix
		}
		rendered[i] = fmt.Sprintf("[%s] %s\n", m.From, text)
	}
	// Collect from the newest backwards while the budget holds.
	var kept []string
	budget := maxBytes
	for i := len(rendered) - 1; i >= 0; i-- {
		if budget-len(rendered[i]) < 0 && len(kept) > 0 {
			kept = append([]string{omittedMarker + "\n"}, kept...)
			break
		}
		if len(rendered[i]) > budget {
			// A single message larger than the whole budget: clip it to
			// what remains rather than dropping the turn entirely.
			rendered[i] = rendered[i][:budget] + clippedSuffix + "\n"
		}
		budget -= len(rendered[i])
		kept = append([]string{rendered[i]}, kept...)
	}
	return strings.Join(kept, "")
}
