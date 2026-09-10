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

func decodeFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(node.Content) == 0 {
		return fmt.Errorf("%s: file is empty", filepath.Base(path))
	}
	doc := node.Content[0]
	if err := decodeStrict(doc, out); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// LoadPipeline reads, strictly decodes, normalizes, and validates a pipeline file.
func LoadPipeline(path string) (*Pipeline, error) {
	var p Pipeline
	if err := decodeFile(path, &p); err != nil {
		return nil, err
	}
	p.normalize()
	if errs := p.Validate(); len(errs) > 0 {
		return nil, errors.New("pipeline " + filepath.Base(path) + ": " + ValidationErrors(errs).Error())
	}
	return &p, nil
}

// LoadRoom reads, strictly decodes, normalizes, and validates a room file.
func LoadRoom(path string) (*Room, error) {
	var r Room
	if err := decodeFile(path, &r); err != nil {
		return nil, err
	}
	r.normalize()
	if errs := r.Validate(); len(errs) > 0 {
		return nil, errors.New("room " + filepath.Base(path) + ": " + ValidationErrors(errs).Error())
	}
	return &r, nil
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
		if s.Agent != nil && s.Agent.Model != nil {
			s.Agent.Model.normalize()
		}
		if s.Agent != nil && s.Agent.Output == "" {
			s.Agent.Output = s.ID
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
