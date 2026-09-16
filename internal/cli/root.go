// Package cli implements loop's command-line surface.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/dotenv"
)

// version is set via -ldflags at release time.
var version = "0.0.0-dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "loop",
		Short:         "Deterministic agentic harness: YAML pipelines + persona chat rooms",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newSetupCmd(),
		newValidateCmd(),
		newInitCmd(),
		newAskCmd(),
		newNewCmd(),
		newRunCmd(),
		newChatCmd(),
		newExecutorCmd(),
		newDoctorCmd(),
	)
	return root
}

// Execute runs the root command and maps errors to exit codes. A .env in
// the working directory (and $LOOP_ENV_FILE, if set) is loaded first so
// every command sees the same credentials.
//
// SIGINT/SIGTERM cancel the command context instead of killing the
// process: a run interrupted mid-stage records its resume point and
// exits non-zero rather than dying without a trace. A second signal
// after that restores the default handler — the next one force-kills.
func Execute() {
	loadDotenv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second ctrl-c must be able to kill
	}()
	if err := NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadDotenv applies .env files: real environment wins, the file fills
// gaps. Missing files are fine; malformed lines warn on stderr.
func loadDotenv() {
	paths := []string{".env"}
	if p := os.Getenv("LOOP_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	for _, path := range paths {
		loaded, warnings, err := dotenv.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", path, err)
			continue
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		if loaded > 0 && path == ".env" {
			// Small confirmation that credentials came from the file —
			// surprises about where a key came from are worse than the
			// one-line notice.
			fmt.Fprintf(os.Stderr, "· loaded %d variable(s) from %s\n", loaded, path)
		}
	}
}
