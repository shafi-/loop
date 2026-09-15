package narrator

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

func stage(id string) *config.Stage {
	return &config.Stage{ID: id, Type: config.StageLLM}
}

func TestNarrationHappyPaths(t *testing.T) {
	var gotPrompt string
	n := &Narrator{
		Provider: llm.NewMockFunc(func(req llm.Request) *llm.Response {
			gotPrompt = req.Messages[0].Content
			return &llm.Response{Text: "\"Requirements drafted with 3 acceptance criteria.\""}
		}),
		Model: "test",
	}
	line := n.StageDone(context.Background(), stage("requirements"), strings.Repeat("doc ", 3000))
	if line != "Requirements drafted with 3 acceptance criteria." {
		t.Errorf("line = %q (quotes must be stripped)", line)
	}
	// Long inputs are truncated before they reach the provider.
	if !strings.Contains(gotPrompt, "(truncated)") {
		t.Errorf("long stage output not truncated:\n%.80s", gotPrompt)
	}
}

func TestNarrationFailureYieldsEmpty(t *testing.T) {
	n := &Narrator{Provider: failing{}, Model: "test", Logf: func(string, ...any) {}}
	if got := n.StageDone(context.Background(), stage("a"), "out"); got != "" {
		t.Errorf("StageDone = %q, want empty on failure", got)
	}
	if got := n.StageFailed(context.Background(), stage("a"), context.DeadlineExceeded); got != "" {
		t.Errorf("StageFailed = %q, want empty on failure", got)
	}
}

func TestMediationFallsBackByContract(t *testing.T) {
	n := &Narrator{Provider: failing{}, Model: "test", Logf: func(string, ...any) {}}
	if got := n.MediateHuman(context.Background(), stage("approval"), "the raw prompt"); got != "" {
		t.Errorf("mediation failure must return empty (engine falls back to raw), got %q", got)
	}
}

func TestMediationIncludesPromptAndChoices(t *testing.T) {
	var gotPrompt string
	n := &Narrator{
		Provider: llm.NewMockFunc(func(req llm.Request) *llm.Response {
			gotPrompt = req.Messages[0].Content
			return &llm.Response{Text: "Approve the OAuth plan? yes/no"}
		}),
		Model: "test",
	}
	out := n.MediateHuman(context.Background(), stage("approval"), "Approve?\n<huge doc>")
	if out != "Approve the OAuth plan? yes/no" {
		t.Errorf("mediated = %q", out)
	}
	if !strings.Contains(gotPrompt, "<huge doc>") || !strings.Contains(gotPrompt, "approval") {
		t.Errorf("mediation prompt lost context:\n%.200s", gotPrompt)
	}
}

type failing struct{}

func (failing) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, context.DeadlineExceeded
}
func (failing) Stream(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, context.DeadlineExceeded
}
