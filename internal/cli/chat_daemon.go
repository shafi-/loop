package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/daemon"
)

// chatViaDaemon attaches to a room hosted by the daemon: messages are
// POSTed, replies arrive as transcript appends over the room's SSE
// stream. The room outlives this terminal — /detach leaves it thinking
// on the server, and reattaching shows what happened in between.
// socket selects which daemon ("" = default, then $LOOP_DAEMON_SOCK).
func chatViaDaemon(cmd *cobra.Command, file, opening, socket string) error {
	cl, err := daemon.Dial(daemon.SocketPath(socket))
	if err != nil {
		return err
	}
	info, err := cl.HostRoom(file)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "room %q on the loop daemon — participants: %s\n", info.Name, strings.Join(info.Agents, ", "))
	if len(info.Pipelines) > 0 {
		fmt.Fprintf(out, "pipelines: %s\n", strings.Join(info.Pipelines, ", "))
	}
	fmt.Fprintln(out, "the room lives on the daemon: /detach and it keeps thinking; reattach any time.")
	fmt.Fprintln(out, "/run <pipeline> · /approve · /status · /halt · /agents · /help · /detach")

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Catch up: what the room said while no one was attached.
	if info.Lines > 0 {
		after := 0
		if info.Lines > 10 {
			after = info.Lines - 10
		}
		if lines, err := cl.RoomTranscript(info.Name, after); err == nil {
			fmt.Fprintln(out, "— while you were away —")
			for _, ln := range lines {
				fmt.Fprintf(out, "[%s] %s\n", ln.From, ln.Text)
			}
		}
	}

	lines, err := cl.RoomStream(ctx, info.Name, info.Lines)
	if err != nil {
		return err
	}
	go func() {
		for ln := range lines {
			fmt.Fprintf(out, "[%s] %s\n", ln.From, ln.Text)
		}
	}()

	if opening != "" {
		if err := cl.Say(info.Name, opening); err != nil {
			fmt.Fprintf(out, "✗ %v\n", err)
		}
	}

	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprint(out, "\n> ")
		line, rerr := in.ReadString('\n')
		if rerr != nil {
			// stdin closed: detach, never kill — the room is not ours.
			fmt.Fprintln(out, "detached — the room keeps living on the daemon.")
			return nil
		}
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		switch {
		case text == "/detach" || text == "/quit" || text == "/exit":
			fmt.Fprintf(out, "detached — room %q keeps living on the daemon.\n", info.Name)
			return nil
		case text == "/help":
			printAttachHelp(out)
		case text == "/agents":
			if cur, ok, _ := cl.Room(info.Name); ok {
				fmt.Fprintf(out, "  %s\n", strings.Join(cur.Agents, ", "))
			}
		case text == "/status":
			if cur, ok, _ := cl.Room(info.Name); ok {
				if len(cur.Runs) == 0 {
					fmt.Fprintln(out, "no active runs — /run <pipeline> starts one")
				}
				for _, ru := range cur.Runs {
					state := "running"
					if ru.Waiting {
						state = "awaiting approval"
					}
					fmt.Fprintf(out, "  %s — run %s [%s] %s\n", ru.Pipeline, ru.RunID, state, ru.LastLine)
				}
			}
		case fields[0] == "/run":
			if len(fields) < 2 {
				fmt.Fprintln(out, "usage: /run <pipeline> [--resume <id>] [--var k=v]…")
				continue
			}
			alias := fields[1]
			var resume string
			var vars []string
			for i := 2; i < len(fields); i++ {
				switch {
				case fields[i] == "--resume" && i+1 < len(fields):
					i++
					resume = fields[i]
				case fields[i] == "--var" && i+1 < len(fields):
					i++
					vars = append(vars, "--var", fields[i])
				default:
					vars = append(vars, fields[i])
				}
			}
			if _, err := cl.RoomRun(info.Name, alias, resume, vars); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			}
		case fields[0] == "/approve":
			if len(fields) < 2 {
				fmt.Fprintln(out, "usage: /approve <yes|no|your words>  (or /approve <pipeline> <answer>)")
				continue
			}
			rest := fields[1:]
			alias := ""
			if len(rest) > 1 {
				alias, rest = rest[0], rest[1:]
			}
			if err := cl.RoomApprove(info.Name, alias, strings.Join(rest, " ")); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			}
		case fields[0] == "/halt":
			alias := ""
			if len(fields) > 1 {
				alias = fields[1]
			}
			if err := cl.RoomHalt(info.Name, alias); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			}
		default:
			if err := cl.Say(info.Name, text); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			}
		}
	}
}

func printAttachHelp(out io.Writer) {
	fmt.Fprintln(out, `  just type    the room hears it; tagged agents reply, others may join
  @name text   force a reply from @name
  /agents      list participants
  /run <name> [--var k=v]…   run one of the room's pipelines
  /approve <yes|no|words>    answer a pipeline asking for approval
  /status      the room's active runs
  /halt [name] pause a run (resumable)
  /detach      leave the session — the room keeps living on the daemon`)
}
