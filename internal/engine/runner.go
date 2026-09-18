package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/executor"
	"github.com/shafi-/loop/internal/usage"
)

// maxSteps caps total stage transitions per run. The schema allows routers
// to jump backward (rework loops), so a cycle guard is the only thing
// standing between a bad pipeline and an infinite run.
const maxSteps = 1000

// Runner executes one pipeline.
type Runner struct {
	Pipeline  *config.Pipeline
	Source    []byte // raw pipeline YAML, snapshotted into the run dir
	Executors *executor.Registry
	Providers ProviderFactory // nil → DefaultProviderFactory
	Human     HumanIO
	Narrator  Narrator  // optional core-loop LLM; nil = silent progress
	Stdout    io.Writer // llm/agent stream target; nil = quiet
	RunsDir   string    // default .loop/runs
	RunID     string    // explicit id; generated when empty
	ResumeID  string    // non-empty: resume this run
	CWD       string    // working dir for tool/agent stages; empty = process cwd
	Logf      func(format string, args ...any)
	// Usage, when set, is the run's token ledger — the caller may
	// pre-populate it (e.g. wrapping the narrator's provider in it
	// before Run). Run creates a fresh meter when nil. Resumed runs
	// accumulate onto the totals their state.json recorded.
	Usage *usage.Meter
}

// RunResult is the terminal state of a run.
type RunResult struct {
	RunID       string
	Completed   bool
	Paused      bool   // stopped at a prompt or by interrupt — resumable, not failed
	PausedStage string // the stage resume will re-run
	FailedStage string
	Err         error
	Summary     string // narrator's failure explanation, when configured
	Steps       int
	Usage       usage.Total // tokens across all sessions of this run (0 when nothing was metered)
}

// resumeState tracks which stages already produced output in a resumed run.
type resumeState map[string]bool

func (r *Runner) runlogf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Run executes the pipeline to completion or first halt-failure.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	resuming := r.ResumeID != ""
	// A fresh run needs a pipeline; a resumed run replays the snapshot.
	if !resuming && r.Pipeline == nil {
		return nil, fmt.Errorf("no pipeline configured")
	}
	deps := &stageDeps{
		Providers: r.Providers,
		Executors: r.Executors,
		Human:     r.Human,
		Pipeline:  r.Pipeline,
		Stdout:    r.Stdout,
		CWD:       r.CWD,
	}
	if deps.Providers == nil {
		deps.Providers = DefaultProviderFactory()
	}
	if deps.Executors == nil {
		deps.Executors = executor.NewRegistry()
	}
	if r.Usage == nil {
		r.Usage = usage.NewMeter()
	}
	deps.Usage = r.Usage

	runID := r.RunID
	if runID == "" {
		runID = newRunID()
	}
	var (
		log *RunLog
		err error
	)
	if resuming {
		log, err = OpenRunLog(r.RunsDir, r.ResumeID)
		if err != nil {
			return nil, err
		}
		runID = r.ResumeID
		// Resume replays the snapshotted pipeline, not a possibly-edited file.
		snap, err := log.readPipelineSnapshot()
		if err != nil {
			log.Close()
			return nil, err
		}
		pipeline, err := config.ParsePipeline(snap)
		if err != nil {
			log.Close()
			return nil, fmt.Errorf("snapshotted pipeline no longer parses: %w", err)
		}
		r.Pipeline = pipeline
		r.Source = snap
		deps.Pipeline = pipeline
	} else {
		log, err = CreateRunLog(r.RunsDir, runID, r.Source)
		if err != nil {
			return nil, err
		}
	}
	defer log.Close()
	deps.Log = log
	deps.Warnf = r.runlogf
	deps.Narrator = r.Narrator

	c := NewContext(r.Pipeline.Vars)
	// persistState saves the resume pointer with the run's accumulated
	// usage: prior sessions (state.json) + this session's meter.
	var priorUsage usage.Total
	persistState := func(s runState) {
		s.Usage = usageTotalPtr(priorUsage.Add(r.Usage.Totals()))
		if err := log.SaveState(s); err != nil {
			r.runlogf("saving state: %v", err)
		}
	}
	indexByID := make(map[string]int, len(r.Pipeline.Stages))
	for i := range r.Pipeline.Stages {
		indexByID[r.Pipeline.Stages[i].ID] = i
	}
	next := 0
	var state runState
	if resuming {
		snap, err := LoadContext(r.RunsDir, runID)
		if err != nil {
			return nil, err
		}
		if snap != nil {
			c = snap
			r.runlogf("resuming run %s with %d prior stage result(s)", runID, len(c.stages))
		}
		st, err := LoadState(r.RunsDir, runID)
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("run %s has no recorded state to resume from", runID)
		}
		if st.Usage != nil {
			priorUsage = *st.Usage
		}
		if st.Done {
			r.runlogf("run %s is already complete — nothing to resume", runID)
			return &RunResult{RunID: runID, Completed: true}, nil
		}
		// Resume re-runs the failed stage, or the paused one (/pause or an
		// interrupt): both recorded their point and neither completed.
		resumeAt := st.Failed
		if resumeAt == "" {
			resumeAt = st.Paused
		}
		if resumeAt == "" {
			return nil, fmt.Errorf("run %s stopped without a recorded failure or pause point and cannot be resumed", runID)
		}
		state = *st
		// Replay semantics: skip the recorded execution path in order (it may
		// revisit stages via rework loops), then re-run the stop point.
		// Everything after that executes fresh.
		for _, id := range state.Path {
			r.runlogf("= %s (already complete, skipping)", id)
			log.Event("stage_skipped", id, nil)
		}
		idx, ok := indexByID[resumeAt]
		if !ok {
			return nil, fmt.Errorf("resume point %q not found in snapshotted pipeline", resumeAt)
		}
		next = idx
		r.runlogf("↺ resuming at stage %q", resumeAt)
	} else {
		log.Event("run_started", "", map[string]any{
			"pipeline": r.Pipeline.Name,
			"resume":   false,
		})
	}

	res := &RunResult{RunID: runID}
	record := func(stageID string) {
		state.Path = append(state.Path, stageID)
		// A completed stage supersedes any stale stop-point label: the
		// state file must tell the truth about the run's last event
		// (a failed label lingering after progress misleads readers).
		state.Failed = ""
		state.Paused = ""
		persistState(state)
	}
	defer func() {
		ev := "run_completed"
		stage := res.PausedStage
		switch {
		case res.Paused:
			ev = "run_paused"
		case !res.Completed:
			ev = "run_failed"
			// The event names where the run stopped: for a failure that is
			// the failed stage, not the (empty) pause pointer.
			stage = res.FailedStage
		}
		res.Usage = priorUsage.Add(r.Usage.Totals())
		if res.Usage.Calls > 0 {
			log.Event("usage", "", map[string]any{
				"calls": res.Usage.Calls, "input_tokens": res.Usage.InputTokens, "output_tokens": res.Usage.OutputTokens,
			})
		}
		log.Event(ev, stage, map[string]any{"steps": res.Steps})
	}()

	for ; next < len(r.Pipeline.Stages) && res.Completed == false; res.Steps++ {
		if res.Steps >= maxSteps {
			res.Completed = false
			res.Err = fmt.Errorf("run exceeded %d stage transitions — your routers likely form a cycle", maxSteps)
			return res, nil
		}
		s := &r.Pipeline.Stages[next]
		if ctx.Err() != nil {
			// An interrupt (ctrl-c) is a pause with a cause: record where to
			// resume — without this, an interrupted run could not be resumed
			// at all (its state recorded no stop point).
			res.Paused = true
			res.PausedStage = s.ID
			res.Err = ctx.Err()
			state.Paused = s.ID
			persistState(state)
			return res, nil
		}

		r.runlogf("→ %s (%s)", s.ID, s.Type)
		// The stage type travels as "stage_type": RunLog merges payload
		// keys over the entry, so a payload "type" would clobber the
		// event kind and the line would no longer say stage_started.
		log.Event("stage_started", s.ID, map[string]any{"stage_type": string(s.Type)})

		outcome, err := r.runWithRetry(ctx, s, c, deps, log)
		if err != nil {
			// A pause (/pause, /quit, /exit at a prompt) is a clean stop:
			// resumable, not a failure, nothing retried.
			if errors.Is(err, ErrPaused) {
				res.Paused = true
				res.PausedStage = s.ID
				state.Paused = s.ID
				state.Failed = ""
				persistState(state)
				return res, nil
			}
			log.Event("stage_failed", s.ID, map[string]any{"error": err.Error()})
			if s.OnError == config.OnErrorSkip {
				r.runlogf("! %s failed, on_error=skip: %v", s.ID, err)
				c.SetOutput(s.ID, "status", "failed")
				record(s.ID)
				log.SaveContext(c)
				next++
				continue
			}
			res.Completed = false
			res.FailedStage = s.ID
			res.Err = fmt.Errorf("stage %s: %w", s.ID, err)
			state.Failed = s.ID
			state.Paused = "" // a failure is the stop point now, not the old pause
			if r.Narrator != nil {
				if summary := r.Narrator.StageFailed(ctx, s, err); summary != "" {
					res.Summary = summary
					log.Event("narration", s.ID, map[string]any{"text": summary})
				}
			}
			persistState(state)
			return res, nil
		}

		// Record outputs: canonical slot, human answer mirror, alias.
		c.SetOutput(s.ID, "output", outcome.Output)
		if s.Type == config.StageHuman {
			c.SetOutput(s.ID, "answer", outcome.Output)
		}
		if alias := stageAlias(s); alias != "" {
			c.outputs[alias] = outcome.Output
		}
		c.SetOutput(s.ID, "status", "done")

		if r.Narrator != nil {
			if line := r.Narrator.StageDone(ctx, s, outcome.Output); line != "" {
				log.Event("narration", s.ID, map[string]any{"text": line})
				r.runlogf("ℹ %s", line)
			}
		}

		if s.Type == config.StageRouter {
			target, ok := indexByID[outcome.Next]
			if !ok {
				res.Completed = false
				res.FailedStage = s.ID
				res.Err = fmt.Errorf("router %s: rule targets unknown stage %q", s.ID, outcome.Next)
				return res, nil
			}
			log.Event("router_decision", s.ID, map[string]any{"rule": outcome.RuleIndex, "next": outcome.Next})
			r.runlogf("⤷ %s routed to %s", s.ID, outcome.Next)
			next = target
		} else {
			next++
			if next >= len(r.Pipeline.Stages) {
				res.Completed = true
				state.Done = true
			}
		}
		if s.Terminal {
			// A terminal stage ends the run here: pipelines with several
			// ending branches (approve ships, reject halts) need no guard
			// routers after each one.
			res.Completed = true
			state.Done = true
			r.runlogf("■ %s is a terminal — run complete", s.ID)
		}
		record(s.ID)
		log.SaveContext(c)
	}

	if res.Completed {
		r.runlogf("✓ run %s complete (%d steps)", runID, res.Steps+1)
	}
	return res, nil
}

// runWithRetry applies the stage's retry policy around its runner.
func (r *Runner) runWithRetry(ctx context.Context, s *config.Stage, c *Context, d *stageDeps, log *RunLog) (*stageOutcome, error) {
	runner, ok := stageRunners[s.Type]
	if !ok {
		return nil, fmt.Errorf("no runner for stage type %q", s.Type)
	}
	attempts := 1
	backoff := time.Duration(0)
	if s.Retry != nil && s.Retry.MaxAttempts > 1 {
		attempts = s.Retry.MaxAttempts
		backoff = time.Duration(s.Retry.BackoffMs) * time.Millisecond
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		outcome, err := runner(ctx, s, c, d)
		if err == nil {
			return outcome, nil
		}
		// A user pause is not a transient error — never retried.
		if errors.Is(err, ErrPaused) {
			return nil, err
		}
		lastErr = err
		if attempt < attempts {
			log.Event("stage_retry", s.ID, map[string]any{"attempt": attempt, "error": err.Error()})
			r.runlogf("↻ %s attempt %d/%d failed: %v", s.ID, attempt, attempts, err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, lastErr
}

// stageAlias returns the explicit output name for alias storage, if any.
func stageAlias(s *config.Stage) string {
	switch s.Type {
	case config.StageLLM:
		if s.LLM.Output != "" && s.LLM.Output != s.ID {
			return s.LLM.Output
		}
	case config.StageAgent:
		if s.Agent.Output != "" && s.Agent.Output != s.ID {
			return s.Agent.Output
		}
	case config.StageTool:
		if s.Tool.Output != "" && s.Tool.Output != s.ID {
			return s.Tool.Output
		}
	}
	return ""
}
