package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/counters"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/narrator"
)

// terminalHuman asks the user questions on stderr and reads replies from
// stdin (stderr keeps stdout clean for stage output streams).
type terminalHuman struct {
	in  *bufio.Reader
	out io.Writer
}

func newTerminalHuman() *terminalHuman {
	return &terminalHuman{in: bufio.NewReader(os.Stdin), out: os.Stderr}
}

// humanLineKind classifies one typed line at a human prompt.
type humanLineKind int

const (
	humanLineAnswer humanLineKind = iota
	humanLineEmpty                // blank line — ask again, it is not an answer
	humanLinePause                // /pause, /quit, /exit — stop the run resumably
)

// pauseCommands are the human-prompt equivalents of the chat room's
// session commands. They pause the run cleanly; a pause is resumable and
// is never mistaken for a stage answer (a stray "not yes" that would
// route the pipeline into a rework loop).
var pauseCommands = map[string]bool{"/pause": true, "/quit": true, "/exit": true}

func classifyHumanLine(line string) humanLineKind {
	t := strings.TrimSpace(line)
	if t == "" {
		return humanLineEmpty
	}
	if pauseCommands[t] {
		return humanLinePause
	}
	return humanLineAnswer
}

func (t *terminalHuman) Prompt(ctx context.Context, prompt string) (string, error) {
	// Empty lines re-ask rather than count as an answer; pause commands
	// stop the run for real.
	for {
		fmt.Fprintf(t.out, "\n── your input needed ──────────────────────\n%s\n", strings.TrimRight(prompt, "\n"))
		fmt.Fprintf(t.out, "(/pause · /quit · /exit pause the run — resumable)\n> ")
		answerCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			line, err := t.in.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			answerCh <- strings.TrimSpace(line)
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-errCh:
			return "", fmt.Errorf("reading answer: %w", err)
		case line := <-answerCh:
			switch classifyHumanLine(line) {
			case humanLineEmpty:
				continue
			case humanLinePause:
				return "", engine.ErrPaused
			}
			return line, nil
		}
	}
}

func newRunCmd() *cobra.Command {
	var (
		vars    []string
		runID   string
		resume  string
		quiet   bool
		runsDir string
		daemon  bool
	)
	cmd := &cobra.Command{
		Use:   "run <pipeline.yaml>",
		Short: "Execute a pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if daemon && runID != "" {
				return fmt.Errorf("--run-id is not supported with --daemon (the daemon assigns ids; use --resume to reattach)")
			}
			if daemon {
				file, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				logf := func(format string, a ...any) {
					fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
				}
				return runViaDaemon(cmd.Context(), logf, file, resume, vars)
			}
			source, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			pipeline, err := config.ParsePipeline(source)
			if err != nil {
				return fmt.Errorf("pipeline %s: %w", args[0], err)
			}
			for _, kv := range vars {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--var expects key=value, got %q", kv)
				}
				pipeline.Vars[k] = v
			}

			// Opt-in, anonymous, local (internal/counters). A resume is a
			// rerun; anything else is a fresh run. Counted at start: an
			// attempt is the event, success or failure is not.
			if resume != "" {
				counters.BumpKey("pipeline_reruns", pipeline.Name)
			} else {
				counters.BumpKey("pipeline_runs", pipeline.Name)
			}

			logf := func(format string, a ...any) {
				if !quiet {
					fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
				}
			}

			// Resolve the run id BEFORE anything consumes it: the runner
			// registers it, the start line announces it — they must agree.
			if resume != "" {
				runID = resume
			} else if runID == "" {
				runID = engine.NewRunID()
			}

			// The narrator is loop's own LLM commentary — opt-in via
			// runtime.narrator. Its failures never affect the run.
			var narr engine.Narrator
			if pipeline.Runtime != nil && pipeline.Runtime.Narrator != nil {
				provider, err := engine.DefaultProviderFactory()(pipeline.Runtime.Narrator)
				if err != nil {
					return fmt.Errorf("narrator: %w", err)
				}
				narr = &narrator.Narrator{
					Provider: provider,
					Model:    pipeline.Runtime.Narrator.Resolve().Model,
					Logf: func(format string, a ...any) {
						fmt.Fprintf(cmd.ErrOrStderr(), "· "+format+"\n", a...)
					},
				}
			}

			runner := &engine.Runner{
				Pipeline:  pipeline,
				Source:    source,
				Executors: executorRegistry(), // cline registered; availability checked at run time
				Human:     newTerminalHuman(),
				Narrator:  narr,
				Stdout:    os.Stdout,
				RunsDir:   runsDir,
				RunID:     runID,
				ResumeID:  resume,
				Logf:      logf,
			}
			logf("run %s starting: %s (%d stages)", runID, pipeline.Name, len(pipeline.Stages))
			res, err := runner.Run(cmd.Context())
			if err != nil {
				return err
			}
			if res.Paused {
				// A pause is a clean stop: state is saved, resume re-runs the
				// paused stage. Interrupts (ctrl-c) keep the failure-style
				// exit code but are just as resumable.
				if res.Err != nil {
					return fmt.Errorf("✗ run %s interrupted at stage %q: %v\n  resume with: loop run %s --resume %s",
						res.RunID, res.PausedStage, res.Err, args[0], res.RunID)
				}
				logf("⏸ run %s paused at stage %q", res.RunID, res.PausedStage)
				fmt.Fprintf(cmd.ErrOrStderr(), "  resume with: loop run %s --resume %s\n", args[0], res.RunID)
				return nil
			}
			if !res.Completed {
				// Failures are never suppressed by --quiet; only progress chatter is.
				var b strings.Builder
				fmt.Fprintf(&b, "✗ run %s failed at stage %q: %v\n", res.RunID, res.FailedStage, res.Err)
				if res.Summary != "" {
					fmt.Fprintf(&b, "  ↳ %s\n", res.Summary)
				}
				fmt.Fprintf(&b, "  resume with: loop run %s --resume %s", args[0], res.RunID)
				return errors.New(b.String())
			}
			logf("✓ run %s completed", res.RunID)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&vars, "var", nil, "override a pipeline var: --var key=value")
	f.StringVar(&runID, "run-id", "", "use a specific run id instead of a generated one")
	f.StringVar(&resume, "resume", "", "resume a previous run by id (replays its snapshotted pipeline)")
	f.StringVar(&runsDir, "runs-dir", "", "where runs are stored (default .loop/runs)")
	f.BoolVarP(&quiet, "quiet", "q", false, "suppress progress lines on stderr")
	f.BoolVar(&daemon, "daemon", false, "submit to a loop serve daemon and attach (the run outlives the terminal)")
	return cmd
}
