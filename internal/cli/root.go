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
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			paths, _ := cmd.Flags().GetStringArray("env-file")
			loadDotenv(paths)
		},
	}
	root.PersistentFlags().StringArray("env-file", nil,
		"environment file(s) to load, replacing the default .env; repeatable, left-to-right")
	root.AddCommand(
		newSetupCmd(),
		newValidateCmd(),
		newInitCmd(),
		newAskCmd(),
		newNewCmd(),
		newRunCmd(),
		newDigestCmd(),
		newServeCmd(),
		newUICmd(),
		newChatCmd(),
		newExecutorCmd(),
		newDoctorCmd(),
	)
	return root
}

// Execute runs the root command and maps errors to exit codes. Env files
// (default `.env`, plus $LOOP_ENV_FILE; or exactly the `--env-file` paths,
// which replace the default) are loaded in PersistentPreRun so the flag is
// parsed first and every command sees the same credentials.
//
// SIGINT/SIGTERM cancel the command context instead of killing the
// process: a run interrupted mid-stage records its resume point and
// exits non-zero rather than dying without a trace. A second signal
// after that restores the default handler — the next one force-kills.
func Execute() {
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

// envPaths decides which files to load: explicit --env-file paths replace
// the default (explicit wins over convention); otherwise .env loads first,
// then $LOOP_ENV_FILE.
func envPaths(explicit []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	paths := []string{".env"}
	if p := os.Getenv("LOOP_ENV_FILE"); p != "" {
		paths = append(paths, p)
	}
	return paths
}

// loadDotenv applies env files: real environment wins, files fill gaps.
// Explicit paths replace the .env default (explicit wins over convention);
// otherwise .env loads first, then $LOOP_ENV_FILE. Missing files are fine;
// malformed lines warn on stderr.
func loadDotenv(explicit []string) {
	for _, path := range envPaths(explicit) {
		loaded, warnings, err := dotenv.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", path, err)
			continue
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		if loaded > 0 {
			// Small confirmation that credentials came from the file —
			// surprises about where a key came from are worse than the
			// one-line notice.
			fmt.Fprintf(os.Stderr, "· loaded %d variable(s) from %s\n", loaded, path)
		}
	}
}
