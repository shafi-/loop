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

// Render formats messages with explicit attribution — the form every
// agent prompt sees ([user] …, [ceo] …).
func Render(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "[%s] %s\n", m.From, m.Text)
	}
	return b.String()
}

// BuildConversation renders the windowed transcript as one attributed
// conversation string for prompts.
func (t *Transcript) BuildConversation(window int) string {
	return Render(t.Window(window))
}
