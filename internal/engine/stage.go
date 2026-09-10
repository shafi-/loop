package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/nerddevsltd/loop/internal/config"
	"github.com/nerddevsltd/loop/internal/executor"
	"github.com/nerddevsltd/loop/internal/llm"
)

// HumanIO is how the engine asks the user a question (human stages).
// Production wires a terminal implementation; tests inject a stub.
type HumanIO interface {
	Prompt(ctx context.Context, prompt string) (string, error)
}

// stageDeps is everything a stage runner may need, injected by the Runner.
type stageDeps struct {
	Providers ProviderFactory
	Executors *executor.Registry
	Human     HumanIO
	Pipeline  *config.Pipeline
	Stdout    io.Writer // streaming target for llm text (nil = quiet)
	CWD       string
}

// stageOutcome is a stage's effect on the run: its textual output plus an
// optional flow override (routers set Next).
type stageOutcome struct {
	Output    string
	Next      string // router: the chosen next stage id
	RuleIndex int    // router: which rule matched (for the event log)
}

// stageRunner executes one typed stage. Errors are stage failures; the
// Runner owns retry and on_error semantics.
type stageRunner func(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error)

var stageRunners = map[config.StageType]stageRunner{
	config.StageLLM:    runLLMStage,
	config.StageTool:   runToolStage,
	config.StageHuman:  runHumanStage,
	config.StageRouter: runRouterStage,
	config.StageAgent:  runAgentStage,
}

// runLLMStage renders the prompt and asks the provider. When Stdout is
// set the response streams to it token-by-token.
func runLLMStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	cfg := s.LLM.Model
	prompt, err := c.Interpolate(s.LLM.Prompt)
	if err != nil {
		return nil, fmt.Errorf("prompt template: %w", err)
	}
	provider, err := d.Providers(cfg)
	if err != nil {
		return nil, err
	}
	req := llmRequest(cfg, s.LLM.System, []llm.Message{{Role: llm.RoleUser, Content: prompt}})

	var text string
	if d.Stdout != nil {
		resp, err := provider.Stream(ctx, req, func(delta string) {
			fmt.Fprint(d.Stdout, delta)
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(d.Stdout)
		text = resp.Text
	} else {
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		text = resp.Text
	}
	return &stageOutcome{Output: text}, nil
}

// runAgentStage delegates to the configured executor. The instruction is
// fully interpolated here: executors receive concrete tasks, never
// templates.
func runAgentStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	exec, ok := d.Executors.Get(s.Agent.Executor)
	if !ok {
		return nil, fmt.Errorf("executor %q is not available (registered: %v) — this build may predate it",
			s.Agent.Executor, d.Executors.Names())
	}
	var persona *config.Persona
	for i := range d.Pipeline.Personas {
		if d.Pipeline.Personas[i].Name == s.Agent.Persona {
			persona = &d.Pipeline.Personas[i]
			break
		}
	}
	if persona == nil {
		return nil, fmt.Errorf("persona %q not found in pipeline", s.Agent.Persona)
	}
	modelCfg := s.Agent.Model
	if modelCfg == nil {
		modelCfg = persona.Model
	}
	if modelCfg == nil {
		return nil, fmt.Errorf("agent stage %q: no model configured on the stage or persona %q", s.ID, persona.Name)
	}

	if s.Agent.Input == "" {
		return nil, fmt.Errorf("agent stage %q needs `input:` — executors receive concrete instructions, not persona musings", s.ID)
	}
	instruction, err := c.Interpolate(s.Agent.Input)
	if err != nil {
		return nil, fmt.Errorf("input template: %w", err)
	}

	task := executor.Task{
		Instruction: instruction,
		System:      persona.System,
		CWD:         d.CWD,
		Model: executor.ModelSpec{
			Provider:    string(modelCfg.Provider),
			Model:       modelCfg.Model,
			BaseURL:     modelCfg.BaseURL,
			Temperature: modelCfg.Temperature,
			MaxTokens:   modelCfg.MaxTokens,
		},
		Tools:    s.Agent.Tools,
		Approval: s.Agent.Approval,
	}
	// API key resolution lives with the provider factory's conventions.
	if key, err := resolveAPIKey(modelCfg); err == nil {
		task.Model.APIKey = key
	}

	res, err := exec.Run(ctx, task, func(ev executor.Event) {
		if ev.Type == executor.EventText && d.Stdout != nil {
			fmt.Fprint(d.Stdout, ev.Text)
		}
	})
	if err != nil {
		return nil, err
	}
	return &stageOutcome{Output: res.Output}, nil
}
