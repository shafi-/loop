package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/nerddevsltd/loop/internal/config"
)

// maxToolOutput caps captured stdout/stderr so a runaway command can't
// exhaust memory. Excess is truncated with a marker.
const maxToolOutput = 1 << 20 // 1 MiB

// runToolStage executes a deterministic local command via the shell and
// captures stdout as the stage output. Non-zero exit is a stage failure
// with the stderr tail attached.
func runToolStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	run, err := c.Interpolate(s.Tool.Run)
	if err != nil {
		return nil, fmt.Errorf("run template: %w", err)
	}
	var stdin string
	if s.Tool.Input != "" {
		stdin, err = c.Interpolate(s.Tool.Input)
		if err != nil {
			return nil, fmt.Errorf("input template: %w", err)
		}
	}
	env := os.Environ()
	for k, v := range s.Tool.Env {
		val, err := c.Interpolate(v)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		env = append(env, k+"="+val)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", run)
	cmd.Env = env
	cmd.Dir = d.CWD
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = maxToolOutput, maxToolOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := err.Error()
		if stderr.Len() > 0 {
			msg += ": " + tail(stderr.String(), 512)
		}
		return nil, fmt.Errorf("command failed: %s", msg)
	}
	// Trailing newlines from `echo` and friends are capture noise, not
	// signal — they would leak into every interpolated prompt.
	return &stageOutcome{Output: strings.TrimRight(stdout.String(), "\n")}, nil
}

// cappedBuffer captures output while discarding bytes beyond a limit —
// a runaway `cat huge.log` must not exhaust memory.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil // always claim success: swallowing excess, not failing
}

func (b *cappedBuffer) Len() int         { return b.buf.Len() }
func (b *cappedBuffer) String() string   { return b.buf.String() }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

var _ io.Writer = (*cappedBuffer)(nil)
