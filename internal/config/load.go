package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ValidationError points at a semantic problem with a config document.
type ValidationError struct {
	Path    string // dotted YAML path, e.g. "stages[2].model.provider"
	Message string
}

func (e ValidationError) Error() string { return e.Path + ": " + e.Message }

// ValidationErrors is the collected failure set from validating one document.
type ValidationErrors []ValidationError

func (es ValidationErrors) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d validation error(s):", len(es))
	for _, e := range es {
		fmt.Fprintf(&b, "\n  - %s: %s", e.Path, e.Message)
	}
	return b.String()
}

// strictDecoder returns a decoder with KnownFields enabled for the given node.
// Strictness is deliberate: in a deterministic tool, a typo'd key must be a
// hard error, not a silently ignored setting.
func strictDecoder(node *yaml.Node) (*yaml.Decoder, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	return dec, nil
}

// unknownFieldRe matches the decoder's internal type rendering so errors
// read "unknown field "modle"" instead of "field modle not found in type
// struct { config.stageCommonYAML ... }".
var unknownFieldRe = regexp.MustCompile(`field (\S+) not found in type .*`)

// decodeStrict strictly decodes a YAML node into out with user-facing errors.
func decodeStrict(node *yaml.Node, out any) error {
	dec, err := strictDecoder(node)
	if err != nil {
		return err
	}
	if err := dec.Decode(out); err != nil {
		var terr *yaml.TypeError
		if errors.As(err, &terr) {
			for i, msg := range terr.Errors {
				terr.Errors[i] = unknownFieldRe.ReplaceAllString(msg, `unknown field ${1}`)
			}
		}
		return err
	}
	return nil
}

// Decode strictly decodes config bytes (YAML or JSON — JSON is a YAML
// subset) into out, with user-facing unknown-field errors. It exists so
// callers that already hold bytes (the NL→pipeline generator) go through
// the exact same strict contract as file loaders.
func Decode(data []byte, out any) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if len(node.Content) == 0 {
		return fmt.Errorf("document is empty")
	}
	return decodeStrict(node.Content[0], out)
}

// ParsePipeline validates pipeline bytes and returns the normalized pipeline.
func ParsePipeline(data []byte) (*Pipeline, error) {
	var p Pipeline
	if err := Decode(data, &p); err != nil {
		return nil, err
	}
	p.normalize()
	if errs := p.Validate(); len(errs) > 0 {
		return nil, ValidationErrors(errs)
	}
	return &p, nil
}

// ParseRoom validates room bytes and returns the normalized room.
func ParseRoom(data []byte) (*Room, error) {
	var r Room
	if err := Decode(data, &r); err != nil {
		return nil, err
	}
	r.normalize()
	if errs := r.Validate(); len(errs) > 0 {
		return nil, ValidationErrors(errs)
	}
	return &r, nil
}

// LoadPipeline reads, strictly decodes, normalizes, and validates a pipeline file.
func LoadPipeline(path string) (*Pipeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParsePipeline(data)
	if err != nil {
		return nil, fmt.Errorf("pipeline %s: %w", filepath.Base(path), err)
	}
	return p, nil
}

// LoadRoom reads, strictly decodes, normalizes, and validates a room file.
func LoadRoom(path string) (*Room, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := ParseRoom(data)
	if err != nil {
		return nil, fmt.Errorf("room %s: %w", filepath.Base(path), err)
	}
	return r, nil
}

// normalize fills provider-dependent defaults. It is separate from Validate
// so validation sees the post-default document, not the raw YAML.
func (p *Pipeline) normalize() {
	for i := range p.Stages {
		p.Stages[i].normalize()
	}
	for i := range p.Personas {
		if p.Personas[i].Model != nil {
			p.Personas[i].Model.normalize()
		}
	}
	if p.Runtime != nil && p.Runtime.Narrator != nil {
		p.Runtime.Narrator.normalize()
	}
}

func (s *Stage) normalize() {
	switch s.Type {
	case StageLLM:
		if s.LLM != nil && s.LLM.Model != nil {
			s.LLM.Model.normalize()
		}
		if s.LLM != nil && s.LLM.Output == "" {
			s.LLM.Output = s.ID
		}
	case StageAgent:
		if s.Agent != nil {
			if s.Agent.Model != nil {
				s.Agent.Model.normalize()
			}
			if s.Agent.Output == "" {
				s.Agent.Output = s.ID
			}
			if s.Agent.Executor == "" {
				s.Agent.Executor = DefaultExecutor
			}
			if s.Agent.Approval == "" {
				s.Agent.Approval = ApprovalAuto
			}
		}
	case StageHuman:
		if s.Human != nil && s.Human.Output == "" {
			s.Human.Output = s.ID + ".answer"
		}
	case StageTool:
		if s.Tool != nil && s.Tool.Output == "" {
			s.Tool.Output = s.ID
		}
	}
}

func (m *ModelConfig) normalize() {
	if m.APIKeyEnv == "" {
		m.APIKeyEnv = DefaultAPIKeyEnv[m.Provider]
	}
}

func (r *Room) normalize() {
	for i := range r.Agents {
		if r.Agents[i].Model != nil {
			r.Agents[i].Model.normalize()
		}
	}
}
