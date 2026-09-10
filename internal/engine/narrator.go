package engine

import (
	"context"

	"github.com/nerddevsltd/loop/internal/config"
)

// Narrator is the engine's hook for its own LLM commentary. All methods
// are best-effort from the engine's perspective: empty strings mean "no
// narration" and never affect run outcomes. Implementations must not
// panic; engine wraps nothing here deliberately — the narrator owns its
// resilience.
type Narrator interface {
	// StageDone narrates a completed stage; "" = no line.
	StageDone(ctx context.Context, s *config.Stage, output string) string
	// StageFailed narrates a halted run; "" = no line.
	StageFailed(ctx context.Context, s *config.Stage, stageErr error) string
	// MediateHuman restates a human stage prompt; "" = use the raw prompt.
	MediateHuman(ctx context.Context, s *config.Stage, prompt string) string
}
