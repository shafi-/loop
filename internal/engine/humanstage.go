package engine

import (
	"context"
	"fmt"

	"github.com/nerddevsltd/loop/internal/config"
)

// runHumanStage pauses the run and asks the user. The answer is stored
// under both `answer` (canonical) and `output` (so downstream references
// need no special cases).
func runHumanStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	prompt, err := c.Interpolate(s.Human.Prompt)
	if err != nil {
		return nil, fmt.Errorf("prompt template: %w", err)
	}
	if d.Human == nil {
		return nil, fmt.Errorf("human stage requires an interactive session (no HumanIO configured)")
	}
	answer, err := d.Human.Prompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return &stageOutcome{Output: answer}, nil
}
