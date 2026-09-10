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
	"time"
)

// Message is one transcript entry.
type Message struct {
	TS   time.Time `json:"ts"`
	From string    `json:"from"` // "user" or an agent name
	Text string    `json:"text"`
}

// Transcript is the room's shared memory: append-only on disk, windowed
// in prompts.
type Transcript struct {
	Messages []Message
	path     string // empty = in-memory only (tests)
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
	t.Messages = append(t.Messages, m)
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

// Window returns at most n recent messages, oldest first.
func (t *Transcript) Window(n int) []Message {
	if n <= 0 || len(t.Messages) <= n {
		return t.Messages
	}
	return t.Messages[len(t.Messages)-n:]
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
