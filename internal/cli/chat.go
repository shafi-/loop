package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/counters"
	"github.com/shafi-/loop/internal/engine"
)

func newChatCmd() *cobra.Command {
	var (
		roomsDir  string
		viaDaemon bool
		socket    string
	)
	cmd := &cobra.Command{
		Use:   "chat <room.yaml> [opening message]",
		Short: "Open a multi-agent chat room (@name to address someone; others decide whether to speak)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if socket != "" && !viaDaemon {
				return fmt.Errorf("--socket only applies with --daemon")
			}
			if viaDaemon {
				file, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				opening := ""
				if len(args) == 2 {
					opening = args[1]
				}
				return chatViaDaemon(cmd, file, opening, socket)
			}
			return runLocalChat(cmd, args, roomsDir)
		},
	}
	cmd.Flags().StringVar(&roomsDir, "rooms-dir", "", "where room transcripts are stored (default .loop/rooms)")
	cmd.Flags().BoolVar(&viaDaemon, "daemon", false, "attach to a room hosted by a loop serve daemon (the room outlives the terminal)")
	cmd.Flags().StringVar(&socket, "socket", "", "with --daemon: which daemon to attach to (default ~/.loop/daemon.sock, override with LOOP_DAEMON_SOCK)")
	return cmd
}

// runLocalChat is the classic in-process room session.
func runLocalChat(cmd *cobra.Command, args []string, roomsDir string) error {
	room, err := config.LoadRoom(args[0])
	if err != nil {
		return err
	}
	agents, err := buildRoomAgents(room)
	if err != nil {
		return err
	}
	if roomsDir == "" {
		roomsDir = filepath.Join(".loop", "rooms")
	}
	transcript, err := chat.OpenTranscript(filepath.Join(roomsDir, room.Name))
	if err != nil {
		return err
	}
	// Opt-in, anonymous, local (internal/counters): one count
	// per opened session, keyed by room name.
	counters.BumpKey("room_sessions", room.Name)
	r := chat.NewRoom(*room, agents, transcript)

	out := cmd.OutOrStdout()
	names := make([]string, 0, len(room.Agents))
	for _, a := range room.Agents {
		names = append(names, a.Name)
	}
	fmt.Fprintf(out, "room %q — participants: %s\n", room.Name, strings.Join(names, ", "))
	fmt.Fprintln(out, "type @name to address someone; untagged agents decide for themselves whether to speak.")
	fmt.Fprintln(out, "/agents list · /help · /quit")

	ui := &terminalChatUI{out: out, err: cmd.ErrOrStderr()}
	// Pipelines this room owns: /run commands real `loop run`
	// subprocesses from inside the conversation.
	bin, err := executablePath()
	if err != nil {
		bin = "loop"
	}
	sess := newRoomRunSession(bin, args[0], room.Pipelines, ui, out, transcript)
	// An optional opening message is delivered exactly as if the
	// user had typed it; the session stays interactive afterwards.
	// Like typed input, a failed delivery never ends the session.
	if len(args) == 2 {
		if err := r.Say(cmd.Context(), args[1], ui); err != nil {
			fmt.Fprintf(ui.err, "✗ %v\n", err)
		}
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprint(out, "\n> ")
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Fprintln(out)
			return nil // stdin closed (ctrl-d) ends the session
		}
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		switch text {
		case "/quit", "/exit":
			sess.Shutdown()
			fmt.Fprintln(out, "session ended — transcript kept in", filepath.Join(roomsDir, room.Name))
			return nil
		case "/help":
			printChatHelp(out)
			continue
		case "/agents":
			for _, a := range room.Agents {
				// Show what would actually run (env/defaults applied).
				rm := a.Model.Resolve()
				tools := "no tools"
				if len(a.Tools) > 0 {
					tools = strings.Join(a.Tools, ", ")
				}
				fmt.Fprintf(out, "  @%s — %s (%s/%s; %s)\n", a.Name, a.Role, rm.Provider, rm.Model, tools)
			}
			continue
		case "/reset":
			if archive, err := r.Rotate(0, "reset"); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			} else {
				fmt.Fprintf(out, "↻ room reset — previous discussion archived as %s\n", archive)
			}
			continue
		case "/fork":
			fields := strings.Fields(text)
			if len(fields) < 2 {
				fmt.Fprintln(out, "usage: /fork <n> — keep the first n transcript lines, archive the rest")
				continue
			}
			through, perr := strconv.Atoi(fields[1])
			if perr != nil || through < 0 {
				fmt.Fprintln(out, "✗ /fork expects a line number (line 1 is the first message)")
				continue
			}
			if archive, err := r.Rotate(through, "fork"); err != nil {
				fmt.Fprintf(out, "✗ %v\n", err)
			} else {
				fmt.Fprintf(out, "↻ forked at line %d — later lines archived as %s\n", through, archive)
			}
			continue
		}
		if handleRoomRunCommand(text, sess, out) {
			continue
		}
		if err := r.Say(cmd.Context(), text, ui); err != nil {
			// Reply failures are real errors (with provider hints
			// already attached) but never end the session.
			fmt.Fprintf(ui.err, "✗ %v\n", err)
		}
	}
}

// executablePath resolves the loop binary for room-commanded runs
// (rooms re-exec the real CLI). A variable so end-to-end tests can
// point it at a freshly built binary.
var executablePath = os.Executable

// buildRoomAgents resolves a provider per agent. An agent without a
// model block is env-driven: whatever family the environment configures
// (ModelConfig.Resolve fills in provider, model id, and key var).
// Agents with tools get the workspace as their working directory.
func buildRoomAgents(room *config.Room) ([]*agent.Agent, error) {
	factory := engine.DefaultProviderFactory()
	cwd, _ := os.Getwd()
	agents := make([]*agent.Agent, 0, len(room.Agents))
	for i := range room.Agents {
		p := room.Agents[i]
		model := p.Model
		if model == nil {
			model = &config.ModelConfig{}
		}
		provider, err := factory(model)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", p.Name, err)
		}
		agents = append(agents, &agent.Agent{Persona: p, Provider: provider, CWD: cwd})
	}
	return agents, nil
}

func printChatHelp(out io.Writer) {
	fmt.Fprintln(out, `  just type    everyone reads it; tagged agents reply, others may join
  @name text   force a reply from @name (multiple tags allowed)
  /agents      list participants
  /pipelines   list the pipelines this room owns
  /run <name> [--var k=v]…   run an owned pipeline in the background
  /approve <yes|no|words>    answer a pipeline asking for approval
  /status      active runs and where their output lives
  /halt [name] stop a run cleanly (resumable with /run <name> --resume <id>)
  /reset       archive the conversation and start a fresh one
  /fork <n>    keep the first n lines, archive the rest — continue from there
  /quit        end the session (running pipelines are halted resumably)`)
}

// handleRoomRunCommand dispatches the pipeline commands. It returns
// false when the line is not one of them, so the room loop can keep
// processing (or send it to the agents).
func handleRoomRunCommand(text string, sess *roomRunSession, out io.Writer) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	switch fields[0] {
	case "/pipelines":
		if len(sess.pipelines) == 0 {
			fmt.Fprintln(out, "this room owns no pipelines — add a `pipelines:` section to the room YAML")
			return true
		}
		for _, p := range sess.pipelines {
			fmt.Fprintf(out, "  %s — %s\n", p.Name, p.File)
		}
		fmt.Fprintln(out, "run one with: /run <name> [--var k=v]…")

	case "/run":
		if len(fields) < 2 {
			fmt.Fprintln(out, "usage: /run <name> [--resume <id>] [--var k=v]…")
			return true
		}
		name := fields[1]
		var resume string
		var extra []string
		for i := 2; i < len(fields); i++ {
			switch fields[i] {
			case "--resume":
				if i+1 < len(fields) {
					i++
					resume = fields[i]
				}
			default:
				extra = append(extra, fields[i])
			}
		}
		if err := sess.Start(name, resume, extra); err != nil {
			fmt.Fprintf(out, "✗ %v\n", err)
		}

	case "/approve":
		if len(fields) < 2 {
			fmt.Fprintln(out, "usage: /approve <yes|no|your words>  (or /approve <name> <answer> when several gates wait)")
			return true
		}
		rest := fields[1:]
		alias := ""
		if len(rest) > 1 {
			if _, known := sess.resolveFile(rest[0]); known {
				alias, rest = rest[0], rest[1:]
			}
		}
		if err := sess.Approve(alias, strings.Join(rest, " ")); err != nil {
			fmt.Fprintf(out, "✗ %v\n", err)
		}

	case "/status":
		for _, line := range sess.Status() {
			fmt.Fprintln(out, line)
		}

	case "/halt":
		alias := ""
		if len(fields) > 1 {
			alias = fields[1]
		}
		if err := sess.Halt(alias); err != nil {
			fmt.Fprintf(out, "✗ %v\n", err)
		}

	default:
		return false
	}
	return true
}

// terminalChatUI renders room activity for a human.
type terminalChatUI struct {
	out io.Writer
	err io.Writer
}

func (u *terminalChatUI) AgentReplyStart(name string) {
	fmt.Fprintf(u.out, "\n── %s ──\n", name)
}

func (u *terminalChatUI) AgentTextDelta(name, delta string) {
	fmt.Fprint(u.out, delta)
}

// AgentsSeen renders read receipts: silence at a glance, like a messaging
// app — one line regardless of room size.
func (u *terminalChatUI) AgentsSeen(names []string) {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "@" + n
	}
	fmt.Fprintf(u.out, "👁 %s saw the message\n", strings.Join(quoted, " "))
}

func (u *terminalChatUI) AgentCapped(name string, priority int) {
	fmt.Fprintf(u.out, "✋ @%s wants to respond too (priority %d, held by max_spontaneous_replies)\n", name, priority)
}

func (u *terminalChatUI) Notice(format string, args ...any) {
	fmt.Fprintf(u.err, "· "+format+"\n", args...)
}
