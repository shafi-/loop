package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/shafi-/loop/internal/daemon"
)

// runViaDaemon is `loop run --daemon`: submit the run to a loop serve
// daemon, then render its event stream locally. The child process and
// its stdin belong to the daemon — the run outlives this terminal, and
// gates are answered by POSTing what the user types. socket selects
// which daemon ("" = the default, then $LOOP_DAEMON_SOCK).
func runViaDaemon(ctx context.Context, logf func(string, ...any), file, resume string, vars []string, socket string) error {
	cl, err := daemon.Dial(daemon.SocketPath(socket))
	if err != nil {
		return err
	}
	sub, err := cl.Submit(file, resume, vars)
	if err != nil {
		return err
	}
	if sub.Warning != "" {
		logf("⚠ %s", sub.Warning)
	}
	logf("run %s starting (daemon): %s", sub.RunID, file)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := cl.Stream(ctx, sub.RunID, sub.EventsSoFar)
	if err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	var failedStage, failedErr string
	for ev := range events {
		var e struct {
			Pipeline  string `json:"pipeline"`
			Stage     string `json:"stage"`
			StageType string `json:"stage_type"`
			Attempt   int    `json:"attempt"`
			Error     string `json:"error"`
			Next      string `json:"next"`
			Text      string `json:"text"`
			Steps     int    `json:"steps"`
		}
		_ = json.Unmarshal(ev.Event, &e)

		switch ev.Type {
		case "stage_started":
			logf("→ %s (%s)", e.Stage, e.StageType)
		case "stage_retry":
			logf("↻ %s attempt %d failed: %s", e.Stage, e.Attempt, e.Error)
		case "router_decision":
			logf("⤷ %s routed to %s", e.Stage, e.Next)
		case "narration":
			logf("ℹ %s", e.Text)
		case "human_prompt":
			// Answer interactively; the typed line posts to the daemon,
			// which writes the child's stdin. Empty input re-asks here
			// (the child re-asks silently, so no new event will arrive).
			for {
				fmt.Fprintf(os.Stderr, "\n── your input needed ──────────────────────\n%s\n", strings.TrimRight(e.Text, "\n"))
				fmt.Fprintf(os.Stderr, "(/pause · /quit · /exit pause the run — resumable)\n> ")
				line, rerr := in.ReadString('\n')
				if rerr != nil {
					return fmt.Errorf("reading answer: %w", rerr)
				}
				answer := strings.TrimSpace(line)
				if answer == "" {
					continue
				}
				if aerr := cl.Answer(sub.RunID, answer); aerr != nil {
					fmt.Fprintf(os.Stderr, "✗ %v\n", aerr)
					continue
				}
				break
			}
		case "stage_failed":
			failedStage, failedErr = e.Stage, e.Error
		case "run_completed":
			logf("✓ run %s complete (%d steps)", sub.RunID, e.Steps)
			return nil
		case "run_paused":
			logf("⏸ run %s paused at stage %q", sub.RunID, e.Stage)
			fmt.Fprintf(os.Stderr, "  resume with: loop run %s --daemon --resume %s\n", file, sub.RunID)
			return nil
		case "run_failed":
			msg := failedErr
			if msg == "" {
				msg = "stage failed"
			}
			return fmt.Errorf("✗ run %s failed at stage %q: %s\n  resume with: loop run %s --daemon --resume %s",
				sub.RunID, failedStage, msg, file, sub.RunID)
		}
	}
	// Stream ended without a terminal event. Ctrl-c lands here too: the
	// context cancels the stream — halt the run so it records a resume
	// point, exactly like an interrupt of a direct run.
	if ctx.Err() != nil {
		_ = cl.Halt(sub.RunID) // best effort: the pause point is what matters
		return fmt.Errorf("✗ run %s interrupted — resume with: loop run %s --daemon --resume %s",
			sub.RunID, file, sub.RunID)
	}
	info, known, err := cl.Run(sub.RunID)
	if err != nil || !known {
		return fmt.Errorf("event stream ended early — see .loop/runs/%s/ on the daemon host", sub.RunID)
	}
	if !info.Alive && !info.Waiting {
		return nil // terminal state recorded while we were between events
	}
	return fmt.Errorf("event stream ended early — the run may still be active (run %s)", sub.RunID)
}
