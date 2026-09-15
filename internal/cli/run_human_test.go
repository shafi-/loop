package cli

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/engine"
)

func TestClassifyHumanLine(t *testing.T) {
	cases := []struct {
		line string
		want humanLineKind
	}{
		{"yes", humanLineAnswer},
		{"  make it shorter  ", humanLineAnswer},
		{"/pause", humanLinePause},
		{"/quit", humanLinePause},
		{"/exit", humanLinePause},
		{"  /quit  ", humanLinePause},
		{"", humanLineEmpty},
		{"   ", humanLineEmpty},
		// Not commands: case differs, or extra words — treated as answers.
		{"/PAUSE", humanLineAnswer},
		{"/quit please", humanLineAnswer},
	}
	for _, c := range cases {
		if got := classifyHumanLine(c.line); got != c.want {
			t.Errorf("classifyHumanLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestTerminalHumanPauseAndEmptyRePrompt(t *testing.T) {
	// Blank lines re-ask; /pause stops the run with engine.ErrPaused.
	h := &terminalHuman{in: bufio.NewReader(strings.NewReader("\n\n/pause\n")), out: &strings.Builder{}}
	_, err := h.Prompt(context.Background(), "Approve?")
	if !errors.Is(err, engine.ErrPaused) {
		t.Fatalf("err = %v, want ErrPaused", err)
	}
	// Three prompts rendered: initial ask plus two blank-line re-asks.
	if n := strings.Count(h.out.(*strings.Builder).String(), "your input needed"); n != 3 {
		t.Errorf("prompt renders = %d, want 3", n)
	}
}

func TestTerminalHumanReturnsAnswerAfterBlanks(t *testing.T) {
	h := &terminalHuman{in: bufio.NewReader(strings.NewReader("\n  \nyes\n")), out: &strings.Builder{}}
	ans, err := h.Prompt(context.Background(), "Approve?")
	if err != nil {
		t.Fatal(err)
	}
	if ans != "yes" {
		t.Fatalf("answer = %q, want yes", ans)
	}
}
