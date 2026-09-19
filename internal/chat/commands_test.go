package chat

import (
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// CommandList is what every "/" menu is built from — the built-in verbs
// with a one-line description each, in registration order.
func TestCommandList(t *testing.T) {
	cfg := config.Room{Name: "c", Agents: []config.Persona{testPersona("solo", "m")}}
	tr, _ := OpenTranscript(t.TempDir())
	r := NewRoom(cfg, nil, tr)

	list := r.CommandList()
	if len(list) < 5 {
		t.Fatalf("command list = %+v, want at least the five built-ins", list)
	}
	got := map[string]string{}
	for _, c := range list {
		if c.Desc == "" {
			t.Errorf("verb %q lacks a description", c.Name)
		}
		got[c.Name] = c.Desc
	}
	for _, want := range []string{"reset", "fork", "cost", "compact", "notes"} {
		if _, ok := got[want]; !ok {
			t.Errorf("built-in verb %q missing from CommandList: %+v", want, list)
		}
	}
}
