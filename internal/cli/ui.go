package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/daemon"
	"github.com/shafi-/loop/internal/webui"
)

func newUICmd() *cobra.Command {
	var (
		addr   string
		socket string
		noOpen bool
	)
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web dashboard for a running loop daemon (runs, timelines, gates)",
		Long: `Serves loop's web UI on the loopback interface and opens it in your
browser. The UI is a client of the loop daemon: it shows runs and
their timelines, and answers gate questions through the daemon —
including for runs submitted from the terminal with --daemon.

Requires a running daemon: start one with ` + "`loop serve`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := daemon.SocketPath(socket)
			handler, err := webui.New(version, path)
			if err != nil {
				return fmt.Errorf("%v\n  start the daemon first: loop serve", err)
			}
			if addr == "" {
				addr = "127.0.0.1:8787"
			}
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("listening on %s: %w", addr, err)
			}
			url := fmt.Sprintf("http://%s", ln.Addr())
			fmt.Fprintf(cmd.OutOrStdout(), "loop ui %s\n  daemon:  %s\n  serving: %s\n", version, path, url)
			fmt.Fprintf(cmd.OutOrStdout(), "ctrl-c to stop (the daemon and its runs are unaffected).\n")
			if !noOpen {
				go openBrowser(cmd.ErrOrStderr(), url)
			}
			return http.Serve(ln, handler)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8787", "listen address (loopback by default — the UI has no auth)")
	cmd.Flags().StringVar(&socket, "socket", "", "daemon socket (default ~/.loop/daemon.sock, override with LOOP_DAEMON_SOCK)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open a browser")
	return cmd
}

// openBrowser is best effort: failing to open a browser must not fail
// the server; the URL is printed regardless.
func openBrowser(errOut io.Writer, url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(errOut, "· could not open a browser (%v) — visit %s\n", err, url)
	}
}
