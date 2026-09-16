package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/daemon"
)

func newServeCmd() *cobra.Command {
	var (
		socket  string
		runsDir string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the loop core as a local daemon (runs outlive terminals; gates answerable from any client)",
		Long: `Runs the loop core as a server on a unix socket. While it is up:

  loop run --daemon <pipeline.yaml>   submit a run and attach to it
                                      (the run outlives the terminal)

Every run's child process and stdin belong to the daemon, so a gate
asked by any run can be answered from any client. The daemon keeps no
state of its own: runs live in .loop/runs/ as always, and stopping the
daemon pauses active runs — resumable exactly like a ctrl-c.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := daemon.SocketPath(socket)
			if path == "" {
				return fmt.Errorf("no daemon socket path available (set --socket or LOOP_DAEMON_SOCK)")
			}
			// Already serving? Report politely instead of stealing the socket.
			if probe, err := daemon.Dial(path); err == nil {
				_ = probe
				fmt.Fprintf(cmd.ErrOrStderr(), "· a loop daemon is already listening on %s\n", path)
				return nil
			} else if st, serr := os.Stat(path); serr == nil && !st.IsDir() {
				_ = os.Remove(path) // stale socket from a dead daemon
			}

			if runsDir == "" {
				runsDir = filepath.Join(".loop", "runs")
			}
			bin, err := executablePath()
			if err != nil {
				return fmt.Errorf("resolving the loop binary: %w", err)
			}
			srv := daemon.New(version, runsDir, bin)

			ln, err := net.Listen("unix", path)
			if err != nil {
				return fmt.Errorf("listening on %s: %w", path, err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "loop daemon %s\n", version)
			fmt.Fprintf(out, "  socket:   %s\n", path)
			fmt.Fprintf(out, "  runs dir: %s\n", runsDir)
			fmt.Fprintf(out, "submit runs with: loop run --daemon <pipeline.yaml>\n")
			fmt.Fprintf(out, "stop with ctrl-c — active runs pause (resumable).\n")

			return srv.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", fmt.Sprintf("daemon socket (default %s, override with LOOP_DAEMON_SOCK)", daemonDefaultSocketHint()))
	cmd.Flags().StringVar(&runsDir, "runs-dir", "", "where runs are stored (default .loop/runs)")
	return cmd
}

func daemonDefaultSocketHint() string {
	p, err := daemon.DefaultSocket()
	if err != nil {
		return ""
	}
	return p
}
