package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/counters"

	"github.com/shafi-/loop/internal/roomkit"
	"github.com/shafi-/loop/internal/usage"
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
	// The session's token ledger: every agent call records into it and
	// /cost reports from it.
	meter := usage.NewMeter()
	if roomsDir == "" {
		roomsDir = config.RoomsDir
	}
	transcript, err := chat.OpenTranscript(filepath.Join(roomsDir, room.Name))
	if err != nil {
		return err
	}
	// Opt-in, anonymous, local (internal/counters): one count
	// per opened session, keyed by room name.
	counters.BumpKey("room_sessions", room.Name)
	r, err := roomkit.Room(room, transcript, meter)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	// Shell commands from room personas are human-in-the-loop, same as
	// a pipeline's `approval: ask`: the command prints, the terminal
	// answers. No answer (EOF) denies — commands never run unasked.
	toolIn := bufio.NewReader(os.Stdin)
	r.ToolApproval = func(ctx context.Context, agentName, tool, detail string) (bool, string, error) {
		fmt.Fprintf(out, "\n── @%s wants to run a command ──────────────────\n%s\n", agentName, detail)
		fmt.Fprint(out, "allow? [y/N] ")
		answer, err := toolIn.ReadString('\n')
		if err != nil {
			fmt.Fprintln(out)
			return false, "no answer (EOF) — command denied", nil
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, "", nil
		}
		return false, "the user declined this command", nil
	}
	// The knowledge layer: agents carry the maintained digest, /notes
	// reads it for zero tokens. The layer only describes landed work —
	// completed pipeline runs refresh it in their own process; session
	// open reconciles it below. Unresolvable provider = off, one notice.
	if err := roomkit.AttachKnowledge(r, meter); err != nil {
		fmt.Fprintf(out, "· knowledge layer off: %v\n", err)
	}

	names := make([]string, 0, len(room.Agents))
	for _, a := range room.Agents {
		names = append(names, a.Name)
	}
	fmt.Fprintf(out, "room %q — participants: %s\n", room.Name, strings.Join(names, ", "))
	fmt.Fprintln(out, "type @name to address someone; untagged agents decide for themselves whether to speak.")
	fmt.Fprintln(out, "/agents list · /help · /quit")

	ui := &terminalChatUI{out: out, err: cmd.ErrOrStderr()}
	// Session-open maintenance: first seed or external edits are caught
	// here; a fresh layer costs zero calls and stays silent.
	if line := r.EnsureKnowledge(cmd.Context()); line != "" {
		fmt.Fprintln(out, line)
	}
	r.FlushUsage() // the knowledge spend lands in the lifetime ledger too
	// Pipelines this room owns: /run commands real `loop run`
	// subprocesses from inside the conversation.
	bin, err := executablePath()
	if err != nil {
		bin = "loop"
	}
	sess := newRoomRunSession(bin, args[0], room.Pipelines, r, ui, out, transcript)
	// Deep dives: gated on the executor being ready; when unavailable
	// the room never offers and /deepdive explains why.
	sess.attachDives()
	// Rotation is vetoed while a run (pipeline or dive) is alive or
	// waiting — the same rule the daemon attaches to these verbs.
	for _, verb := range []string{"reset", "fork"} {
		if c := r.Command(verb); c != nil {
			c.Guard = func([]string) error { return sess.runsBusy() }
		}
	}
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
		if text == "/" {
			// The discoverability affordance: a bare slash lists every
			// command this surface understands.
			printSlashCommands(out, r, sess)
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
		}
		if handleRoomRunCommand(text, sess, out) {
			continue
		}
		// Announce slow turns and slow tools while the (blocking) turn
		// runs; the tracker's 2s threshold keeps this quiet otherwise.
		watched := make(chan struct{})
		go watchActivity(watched, r, ui)
		if err := r.Say(cmd.Context(), text, ui); err != nil {
			// Reply failures are real errors (with provider hints
			// already attached) but never end the session.
			fmt.Fprintf(ui.err, "✗ %v\n", err)
		}
		close(watched)
	}
}

// executablePath resolves the loop binary for room-commanded runs
// (rooms re-exec the real CLI). A variable so end-to-end tests can
// point it at a freshly built binary.
var executablePath = os.Executable

// printRunnable lists what /run accepts, one per line, marked by kind.
func printRunnable(out io.Writer, names []string) {
	if len(names) == 0 {
		fmt.Fprintln(out, "nothing runnable — add a pipelines: section to the room or create .loop/pipelines/*.yaml")
		return
	}
	for _, n := range names {
		kind := "room alias"
		if strings.Contains(n, "/") {
			kind = "workspace pipeline"
		}
		fmt.Fprintf(out, "  %s (%s)\n", n, kind)
	}
}

// printSlashCommands lists what the user can type after "/": the room
// engine's verbs (every surface shares them), this session's pipeline
// commands (when the room owns pipelines), and the REPL's own verbs.
func printSlashCommands(out io.Writer, r *chat.Room, sess *roomRunSession) {
	fmt.Fprintln(out, "commands:")
	for _, c := range r.CommandList() {
		fmt.Fprintf(out, "  /%-10s %s\n", c.Name, c.Desc)
	}
	if len(sess.pipelines) > 0 {
		fmt.Fprintln(out, "  /run <name> [--var k=v]…   run a pipeline — room alias or path")
		fmt.Fprintln(out, "  /approve <yes|no|words>    answer a pipeline asking for approval")
		fmt.Fprintln(out, "  /status      active runs · /halt [name] stop one resumably")
		fmt.Fprintln(out, "  /pipelines   list this room's pipelines")
	}
	fmt.Fprintln(out, "  /help        the full cheat sheet · /agents  who is in the room")
	fmt.Fprintln(out, "  /quit        end the session (running pipelines are halted resumably)")
}

func printChatHelp(out io.Writer) {
	fmt.Fprintln(out, `  just type    everyone reads it; tagged agents reply, others may join
  @name text   force a reply from @name (multiple tags allowed)
  /            list available commands
  /agents      list participants
  /pipelines   list the pipelines this room owns
  /run <name> [--var k=v]…   run a pipeline — room alias or .loop/pipelines/*.yaml path
  /approve <yes|no|words>    answer a pipeline asking for approval
  /status      active runs and where their output lives
  /halt [name] stop a run cleanly (resumable with /run <name> --resume <id>)
  /reset       archive the conversation and start a fresh one
  /fork <n>    keep the first n lines, archive the rest — continue from there
  /notes [slug]  project knowledge: list the area notes, or read one
  /deepdive @agent [topic]  launch an executor-backed deep-dive run
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
		printRunnable(out, sess.Runnable())
		fmt.Fprintln(out, "run one with: /run <name> [--var k=v]…")

	case "/run":
		if len(fields) < 2 {
			fmt.Fprintln(out, "usage: /run <alias-or-path> [--resume <id>] [--var k=v]…")
			printRunnable(out, sess.Runnable())
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
			if _, _, known := sess.resolveFile(rest[0]); known {
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

// terminalChatUI renders room activity for a human. The activity
// watcher goroutine (see watchActivity) writes alongside the turn
// goroutine, so every write is serialized.
type terminalChatUI struct {
	mu  sync.Mutex
	out io.Writer
	err io.Writer
}

func (u *terminalChatUI) AgentReplyStart(name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.out, "\n── %s ──\n", name)
}

func (u *terminalChatUI) AgentTextDelta(name, delta string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.out, delta)
}

// AgentsSeen renders read receipts: silence at a glance, like a messaging
// app — one line regardless of room size.
func (u *terminalChatUI) AgentsSeen(names []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "@" + n
	}
	fmt.Fprintf(u.out, "👁 %s saw the message\n", strings.Join(quoted, " "))
}

func (u *terminalChatUI) AgentCapped(name string, priority int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.out, "✋ @%s wants to respond too (priority %d, held by max_spontaneous_replies)\n", name, priority)
}

func (u *terminalChatUI) Notice(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.err, "· "+format+"\n", args...)
}

// activityLine is the activity watcher's writer: the same locked path,
// on stderr with the notice accent.
func (u *terminalChatUI) activityLine(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprintf(u.err, "· "+format+"\n", args...)
}

// activityAnnounce decides one watcher tick: the line to print (when
// the activity is new and announce-worthy) and the new last-seen key.
// Per-agent "thinking" is never announced — the reply header
// (── @architect ──) already announces it.
func activityAnnounce(last string, act chat.Activity) (string, string, bool) {
	if act.What == "" {
		return last, "", false
	}
	key := act.Agent + "\x00" + act.What
	if key == last {
		return last, "", false
	}
	if act.Agent != "" && act.What == "thinking" {
		return last, "", false
	}
	if act.Agent != "" {
		return key, "🔧 @" + act.Agent + " " + act.What, true
	}
	return key, act.What, true
}

// watchActivity announces what the room is doing while the turn runs:
// each new visible activity (the 2s announce threshold lives in the
// tracker) prints once, on stderr, so a silent turn is never mistaken
// for a hung one. Per-agent "thinking" is skipped — the reply header
// (── @architect ──) already announces it.
func watchActivity(done <-chan struct{}, r *chat.Room, ui *terminalChatUI) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			act, ok := r.Activity()
			if !ok {
				continue
			}
			var line string
			var announce bool
			last, line, announce = activityAnnounce(last, act)
			if announce {
				ui.activityLine("%s", line)
			}
		}
	}
}
