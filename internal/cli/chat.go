package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
)

func newChatCmd() *cobra.Command {
	var roomsDir string
	cmd := &cobra.Command{
		Use:   "chat <room.yaml> [opening message]",
		Short: "Open a multi-agent chat room (@name to address someone; others decide whether to speak)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
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
					fmt.Fprintln(out, "session ended — transcript kept in", filepath.Join(roomsDir, room.Name))
					return nil
				case "/help":
					printChatHelp(out)
					continue
				case "/agents":
					for _, a := range room.Agents {
						// Show what would actually run (env/defaults applied).
						rm := a.Model.Resolve()
						fmt.Fprintf(out, "  @%s — %s (%s/%s)\n", a.Name, a.Role, rm.Provider, rm.Model)
					}
					continue
				}
				if err := r.Say(cmd.Context(), text, ui); err != nil {
					// Reply failures are real errors (with provider hints
					// already attached) but never end the session.
					fmt.Fprintf(ui.err, "✗ %v\n", err)
				}
			}
		},
	}
	cmd.Flags().StringVar(&roomsDir, "rooms-dir", "", "where room transcripts are stored (default .loop/rooms)")
	return cmd
}

// buildRoomAgents resolves a provider per agent. An agent without a
// model block is env-driven: whatever family the environment configures
// (ModelConfig.Resolve fills in provider, model id, and key var).
func buildRoomAgents(room *config.Room) ([]*agent.Agent, error) {
	factory := engine.DefaultProviderFactory()
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
		agents = append(agents, &agent.Agent{Persona: p, Provider: provider})
	}
	return agents, nil
}

func printChatHelp(out io.Writer) {
	fmt.Fprintln(out, `  just type    everyone reads it; tagged agents reply, others may join
  @name text   force a reply from @name (multiple tags allowed)
  /agents      list participants
  /quit        end the session (transcript is kept)`)
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
