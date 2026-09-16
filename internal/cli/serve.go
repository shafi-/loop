package cli

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/daemon"
	"github.com/shafi-/loop/internal/webui"
)

func newServeCmd() *cobra.Command {
	var (
		socket  string
		runsDir string
		port    int
		noOpen  bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the loop core as a local daemon (runs outlive terminals; gates answerable from any client)",
		Long: `Runs the loop core as a server on a unix socket. While it is up:

  loop run --daemon <pipeline.yaml>   submit a run and attach to it
                                      (the run outlives the terminal)

Add --port to make it the dashboard too — daemon and web UI in one
process, one per project:

  cd ~/work/webapp   && loop serve --port 8787
  cd ~/work/shop-api && loop serve --port 8788

Every run's child process and stdin belong to the daemon, so a gate
asked by any run can be answered from any client. The daemon keeps no
state of its own: runs live in .loop/runs/ as always, and stopping the
daemon pauses active runs — resumable exactly like a ctrl-c.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port expects a TCP port (1-65535), got %d", port)
			}
			var path string
			if port > 0 {
				if socket != "" {
					return fmt.Errorf("--socket and --port both name the daemon; pick one (--port also serves the web UI)")
				}
				p, err := daemon.PortSocket(port)
				if err != nil {
					return err
				}
				path = p
			} else {
				path = daemon.SocketPath(socket)
			}
			if path == "" {
				return fmt.Errorf("no daemon socket path available (set --socket or LOOP_DAEMON_SOCK)")
			}
			// The socket lives under ~/.loop — create it on a fresh
			// machine rather than failing the first serve with a bare
			// bind error.
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
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
			ws, _ := os.Getwd()
			fmt.Fprintf(out, "loop daemon %s\n", version)
			fmt.Fprintf(out, "  workspace: %s\n", filepath.Base(ws))
			fmt.Fprintf(out, "  socket:   %s\n", path)
			fmt.Fprintf(out, "  runs dir: %s\n", runsDir)

			// The API must be accepting before the UI (a client of it)
			// dials in; the unix socket's backlog holds connections
			// until Serve starts accepting, so starting it first is safe.
			ctx := cmd.Context()
			errCh := make(chan error, 1)
			go func() { errCh <- srv.Serve(ctx, ln) }()

			if port > 0 {
				handler, err := webui.New(version, path)
				if err != nil {
					ln.Close()
					<-errCh
					return fmt.Errorf("starting the web UI: %w", err)
				}
				addr := fmt.Sprintf("127.0.0.1:%d", port)
				uiLn, err := net.Listen("tcp", addr)
				if err != nil {
					ln.Close()
					<-errCh
					if portTaken(err) {
						return fmt.Errorf("port %d is already in use — a loop dashboard may be serving there; pick another --port", port)
					}
					return fmt.Errorf("listening on %s: %w", addr, err)
				}
				go func() { _ = http.Serve(uiLn, handler) }()
				url := "http://" + addr
				fmt.Fprintf(out, "  ui:       %s\n", url)
				if !noOpen {
					go openBrowser(cmd.ErrOrStderr(), url)
				}
			}
			fmt.Fprintf(out, "submit runs with: loop run --daemon <pipeline.yaml>\n")
			fmt.Fprintf(out, "stop with ctrl-c — active runs pause (resumable).\n")

			return <-errCh
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "also serve the web dashboard on 127.0.0.1:PORT (daemon + dashboard in one process)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "with --port: do not open a browser")
	cmd.Flags().StringVar(&socket, "socket", "", fmt.Sprintf("daemon socket (default %s, override with LOOP_DAEMON_SOCK)", daemonDefaultSocketHint()))
	cmd.Flags().StringVar(&runsDir, "runs-dir", "", "where runs are stored (default .loop/runs)")
	return cmd
}

// portTaken reports whether a TCP bind failed because the port is
// occupied — the common case being another loop dashboard.
func portTaken(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return errors.Is(oe.Err, syscall.EADDRINUSE)
	}
	return false
}

func daemonDefaultSocketHint() string {
	p, err := daemon.DefaultSocket()
	if err != nil {
		return ""
	}
	return p
}
