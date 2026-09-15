package config

import "fmt"

// allowedRoomTools is the tool set room agents may use natively. Pipeline
// agent stages delegate to their executor, whose tool surface is the
// executor's own contract; rooms execute in-process, so the set is fixed.
var allowedRoomTools = map[string]bool{
	"read_file":   true,
	"write_file":  true,
	"run_command": true,
}

// Validate checks semantics after normalization and returns every problem it
// finds. Syntax and unknown fields are rejected earlier, at decode time.
func (p *Pipeline) Validate() ValidationErrors {
	var errs ValidationErrors
	err := func(path, format string, args ...any) {
		errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if p.Name == "" {
		err("name", "is required")
	}
	if len(p.Stages) == 0 {
		err("stages", "must list at least one stage")
	}

	ids := make(map[string]int, len(p.Stages))
	for i := range p.Stages {
		s := &p.Stages[i]
		path := fmt.Sprintf("stages[%d]", i)
		if s.ID == "" {
			err(path+".id", "is required")
		} else if !validIdent(s.ID) {
			err(path+".id", "must be lowercase letters, digits, '-' or '_' (got %q)", s.ID)
		} else if prev, dup := ids[s.ID]; dup {
			err(path+".id", "duplicates stages[%d].id %q", prev, s.ID)
		} else {
			ids[s.ID] = i
		}

		switch s.Type {
		case StageLLM:
			validateModel(err, path+".model", s.LLM.Model)
			if s.LLM.Prompt == "" {
				err(path+".prompt", "is required for an llm stage")
			}
		case StageAgent:
			if s.Agent.Persona == "" {
				err(path+".persona", "is required for an agent stage")
			}
			switch s.Agent.Approval {
			case "", ApprovalAuto, ApprovalAsk:
			default:
				err(path+".approval", "must be %q or %q (got %q)", ApprovalAuto, ApprovalAsk, s.Agent.Approval)
			}
			validateModel(err, path+".model", s.Agent.Model) // optional: persona may carry one
			if s.Agent.MaxIterations < 0 {
				err(path+".max_iterations", "must not be negative")
			}
		case StageTool:
			if s.Tool.Run == "" {
				err(path+".run", "is required for a tool stage")
			}
		case StageHuman:
			if s.Human.Prompt == "" {
				err(path+".prompt", "is required for a human stage")
			}
			validateModel(err, path+".model", s.Human.Model) // optional: gate classification
			if s.Human.Model != nil && !s.Human.Gate {
				err(path+".model", "is only used with gate: true on a human stage")
			}
		case StageRouter:
			if len(s.Router.When) == 0 {
				err(path+".when", "must list at least one rule")
			}
			if s.Terminal {
				err(path+".terminal", "does not apply to a router — a router's job is to jump, not to end")
			}
			hasDefault := false
			for j := range s.Router.When {
				rule := &s.Router.When[j]
				rp := fmt.Sprintf("%s.when[%d]", path, j)
				if rule.Next == "" {
					err(rp+".next", "is required")
				}
				if rule.If == "" {
					hasDefault = true
				}
			}
			if !hasDefault {
				err(path+".when", "should include one default rule (no \"if\") so the router always has a next stage")
			}
		}

		if s.OnError != "" && s.OnError != OnErrorHalt && s.OnError != OnErrorSkip {
			err(path+".on_error", "must be %q or %q (got %q)", OnErrorHalt, OnErrorSkip, s.OnError)
		}
		if s.Retry != nil {
			if s.Retry.MaxAttempts < 0 {
				err(path+".retry.max_attempts", "must not be negative")
			}
			if s.Retry.BackoffMs < 0 {
				err(path+".retry.backoff_ms", "must not be negative")
			}
		}
	}

	// Router targets may reference any stage (forward jumps allowed for rework loops).
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type != StageRouter {
			continue
		}
		for j := range s.Router.When {
			next := s.Router.When[j].Next
			if _, ok := ids[next]; !ok && next != "" {
				errs = append(errs, ValidationError{
					Path:    fmt.Sprintf("stages[%d].when[%d].next", i, j),
					Message: fmt.Sprintf("references unknown stage %q", next),
				})
			}
		}
	}

	names := make(map[string]bool, len(p.Personas))
	for i := range p.Personas {
		pers := &p.Personas[i]
		path := fmt.Sprintf("personas[%d]", i)
		if pers.Name == "" {
			err(path+".name", "is required")
		} else if !validIdent(pers.Name) {
			err(path+".name", "must be lowercase letters, digits, '-' or '_' (got %q)", pers.Name)
		} else if names[pers.Name] {
			err(path+".name", "duplicates persona %q", pers.Name)
		} else {
			names[pers.Name] = true
		}
		validateModel(err, path+".model", pers.Model)
	}

	if p.Runtime != nil && p.Runtime.Narrator != nil {
		validateModel(err, "runtime.narrator", p.Runtime.Narrator)
	}

	return errs
}

// validateModel checks a ModelConfig when present. Everything about it is
// optional: an omitted block, or an omitted provider inside one, falls
// back to the env-configured family at resolution time
// (ModelConfig.Resolve). Only contradictions are errors — e.g. a
// misspelled provider.
func validateModel(err func(string, string, ...any), path string, m *ModelConfig) {
	if m == nil {
		return
	}
	if m.Provider != "" && m.Provider != ProviderAnthropic && m.Provider != ProviderOpenAI {
		err(path+".provider", "must be one of: anthropic, openai (got %q)", m.Provider)
	}
	if m.Temperature != nil && (*m.Temperature < 0 || *m.Temperature > 2) {
		err(path+".temperature", "must be between 0 and 2 (got %v)", *m.Temperature)
	}
	if m.MaxTokens < 0 {
		err(path+".max_tokens", "must not be negative")
	}
}

// Validate checks room semantics after normalization.
func (r *Room) Validate() ValidationErrors {
	var errs ValidationErrors
	err := func(path, format string, args ...any) {
		errs = append(errs, ValidationError{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	if r.Name == "" {
		err("name", "is required")
	}
	if len(r.Agents) == 0 {
		err("agents", "must list at least one agent")
	}
	pipes := make(map[string]bool, len(r.Pipelines))
	for i := range r.Pipelines {
		p := &r.Pipelines[i]
		path := fmt.Sprintf("pipelines[%d]", i)
		if p.Name == "" {
			err(path+".name", "is required")
		} else if !validIdent(p.Name) {
			err(path+".name", "must be lowercase letters, digits, '-' or '_' (got %q)", p.Name)
		} else if pipes[p.Name] {
			err(path+".name", "duplicates pipeline %q", p.Name)
		} else {
			pipes[p.Name] = true
		}
		if p.File == "" {
			err(path+".file", "is required")
		}
	}
	seen := make(map[string]bool, len(r.Agents))
	for i := range r.Agents {
		a := &r.Agents[i]
		path := fmt.Sprintf("agents[%d]", i)
		if a.Name == "" {
			err(path+".name", "is required")
		} else if !validIdent(a.Name) {
			err(path+".name", "must be lowercase letters, digits, '-' or '_' (got %q)", a.Name)
		} else if seen[a.Name] {
			err(path+".name", "duplicates agent %q", a.Name)
		} else {
			seen[a.Name] = true
		}
		validateModel(err, path+".model", a.Model)
		for _, tool := range a.Tools {
			// Rooms execute tools natively; the set is small on purpose.
			if !allowedRoomTools[tool] {
				err(path+".tools", "%q is not a room tool (available: read_file, write_file, run_command)", tool)
			}
		}
	}
	if r.Settings.SpeakThreshold < 0 || r.Settings.SpeakThreshold > 1 {
		err("settings.speak_threshold", "must be between 0 and 1 (got %v)", r.Settings.SpeakThreshold)
	}
	if r.Settings.MaxSpontaneousReplies < 0 {
		err("settings.max_spontaneous_replies", "must not be negative")
	}
	if r.Settings.HistoryWindow < 0 {
		err("settings.history_window", "must not be negative")
	}
	return errs
}
