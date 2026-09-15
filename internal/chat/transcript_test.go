package chat

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The transcript is appended from the room loop, pipeline status relays,
// and agent tool hooks concurrently — pin that under -race, on disk too.
func TestTranscriptConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := tr.Append("agent", "msg"); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	if len(tr.Messages) != writers*each {
		t.Fatalf("messages = %d, want %d", len(tr.Messages), writers*each)
	}
	// Every message also persisted.
	data, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := countLines(string(data)); n != writers*each {
		t.Errorf("persisted lines = %d, want %d", n, writers*each)
	}
	// Windowing while appending stays race-free.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = tr.BuildConversation(10)
		}
	}()
	_ = tr.Append("user", "x")
	<-done
}

func countLines(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	return n
}
