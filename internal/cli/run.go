package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/engine"
)

// terminalHuman asks the user questions on stderr and reads replies from
// stdin (stderr keeps stdout clean for stage output streams).
type terminalHuman struct {
	in  *bufio.Reader
	out *os.File
}

func newTerminalHuman() *terminalHuman {
	return &terminalHuman{in: bufio.NewReader(os.Stdin), out: os.Stderr}
}

func (t *terminalHuman) Prompt(ctx context.Context, prompt string) (string, error) {
	fmt.Fprintf(t.out, "\n── your input needed ──────────────────────\n%s\n> ", strings.TrimRight(prompt, "\n"))
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
	case a := <-answerCh:
		return a, nil
	}
}

func newRunCmd() *cobra.Command {
	var (
		vars    []string
		runID   string
		resume  string
		quiet   bool
		runsDir string
	)
	cmd := &cobra.Command{
		Use:   "run <pipeline.yaml>",
		Short: "Execute a pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			logf := func(format string, a ...any) {
				if !quiet {
					fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...)
				}
			}
			runner := &engine.Runner{
				Pipeline:  pipeline,
				Source:    source,
				Executors: executorRegistry(), // cline registered; availability checked at run time
				Human:     newTerminalHuman(),
				Stdout:    os.Stdout,
				RunsDir:   runsDir,
				RunID:     runID,
				ResumeID:  resume,
				Logf:      logf,
			}
			id := resume
			if id == "" {
				if runID == "" {
					runID = engine.NewRunID()
				}
				id = runID
			}
			logf("run %s starting: %s (%d stages)", id, pipeline.Name, len(pipeline.Stages))
			res, err := runner.Run(cmd.Context())
			if err != nil {
				return err
			}
			if !res.Completed {
				logf("✗ run %s failed at stage %q: %v", res.RunID, res.FailedStage, res.Err)
				fmt.Fprintf(cmd.ErrOrStderr(), "  resume with: loop run %s --resume %s\n", args[0], res.RunID)
				os.Exit(1)
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
	return cmd
}
