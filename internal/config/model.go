// Package config defines the YAML contract for loop pipelines and rooms,
// loads them strictly (unknown fields are errors), applies defaults, and
// validates semantics. This schema is the product's user-facing API.
package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// StageType discriminates what a stage does at runtime.
type StageType string

const (
	StageLLM    StageType = "llm"    // single templated completion
	StageAgent  StageType = "agent"  // persona + tools + agentic loop
	StageTool   StageType = "tool"   // deterministic local command
	StageHuman  StageType = "human"  // pause and ask the user
	StageRouter StageType = "router" // deterministic branch on context
)

// Provider identifies an LLM API family.
type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai" // OpenAI API shape, incl. base_url-compatible services
)

// ValidProviders lists the providers v1 supports.
var ValidProviders = []Provider{ProviderAnthropic, ProviderOpenAI}

// DefaultAPIKeyEnv is the fallback env var consulted when api_key_env is unset.
var DefaultAPIKeyEnv = map[Provider]string{
	ProviderAnthropic: "ANTHROPIC_API_KEY",
	ProviderOpenAI:    "OPENAI_API_KEY",
}

// ModelConfig selects which LLM a stage, agent, or narrator uses. The
// optional pieces (model id, base_url) resolve via Resolve():
// caller value > env override > built-in default.
type ModelConfig struct {
	Provider    Provider `yaml:"provider"`
	Model       string   `yaml:"model"`                  // optional; see Resolve()
	BaseURL     string   `yaml:"base_url,omitempty"`     // redirects the provider's API family
	APIKeyEnv   string   `yaml:"api_key_env,omitempty"`  // env var holding the key
	Temperature *float64 `yaml:"temperature,omitempty"`  // nil = provider default
	MaxTokens   int      `yaml:"max_tokens,omitempty"`   // 0 = adapter default
}

// RetryPolicy controls per-stage retry on transient failures.
type RetryPolicy struct {
	MaxAttempts int `yaml:"max_attempts,omitempty"` // 0 = no retry (1 attempt)
	BackoffMs   int `yaml:"backoff_ms,omitempty"`   // base backoff, doubles per attempt
}

// OnError values for Stage.OnError.
const (
	OnErrorHalt = "halt" // default: stop the run
	OnErrorSkip = "skip" // record failure, continue to next stage
)

// LLMStage is a single completion from a templated prompt.
type LLMStage struct {
	Model  *ModelConfig `yaml:"model"`
	Prompt string       `yaml:"prompt"`
	System string       `yaml:"system,omitempty"`
	Output string       `yaml:"output,omitempty"` // context key; defaults to the stage id
}

// AgentStage runs a persona as an agent with tools until it finishes
// or exhausts MaxIterations. Execution is delegated to a pluggable
// executor (v1: the "cline" executor).
type AgentStage struct {
	Persona       string       `yaml:"persona"`
	Input         string       `yaml:"input,omitempty"`
	Executor      string       `yaml:"executor,omitempty"` // default: cline
	Approval      string       `yaml:"approval,omitempty"` // auto (default) | ask
	Model         *ModelConfig `yaml:"model,omitempty"`    // overrides the persona's model
	Tools         []string     `yaml:"tools,omitempty"`
	MaxIterations int          `yaml:"max_iterations,omitempty"` // 0 = engine default
	Output        string       `yaml:"output,omitempty"`
}

// Valid approval policies for agent stages.
const (
	ApprovalAuto = "auto"
	ApprovalAsk  = "ask"
)

// DefaultExecutor is used when an agent stage does not name one.
const DefaultExecutor = "cline"

// ToolStage runs a deterministic local command.
type ToolStage struct {
	Run    string            `yaml:"run"`
	Input  string            `yaml:"input,omitempty"` // templated text piped to stdin
	Env    map[string]string `yaml:"env,omitempty"`   // extra env vars (values may be templated)
	Output string            `yaml:"output,omitempty"`
}

// HumanStage pauses the run and asks the user a question in the terminal.
type HumanStage struct {
	Prompt string `yaml:"prompt"`
	Output string `yaml:"output,omitempty"` // context key for the answer; defaults to "<stage id>.answer"
}

// RouteRule is one router arm. A rule with an empty If is the default arm.
type RouteRule struct {
	If   string `yaml:"if,omitempty"` // expression over the run context
	Next string `yaml:"next"`         // id of the stage to jump to
}

// RouterStage overrides linear flow by branching on the context.
type RouterStage struct {
	When []RouteRule `yaml:"when"`
}

// Stage is one step of a pipeline. Type-specific payloads decode strictly:
// unknown fields under a stage are a load error, not a silent ignore.
type Stage struct {
	ID      string
	Type    StageType
	Retry   *RetryPolicy
	OnError string // OnErrorHalt | OnErrorSkip

	LLM    *LLMStage
	Agent  *AgentStage
	Tool   *ToolStage
	Human  *HumanStage
	Router *RouterStage
}

type stageCommonYAML struct {
	ID      string       `yaml:"id"`
	Retry   *RetryPolicy `yaml:"retry,omitempty"`
	OnError string       `yaml:"on_error,omitempty"`
}

// UnmarshalYAML implements yaml.Unmarshaler with a two-phase decode: peek at
// `type`, then strictly decode the whole node into that type's flat layout.
func (s *Stage) UnmarshalYAML(node *yaml.Node) error {
	var probe struct {
		Type string `yaml:"type"`
	}
	if err := node.Decode(&probe); err != nil {
		return err
	}
	if probe.Type == "" {
		return fmt.Errorf("stage is missing required field %q", "type")
	}

	// Re-decode the node strictly into that type's flat layout: fields of
	// other stage types are rejected, not silently ignored.
	switch StageType(probe.Type) {
	case StageLLM:
		var v struct {
			stageCommonYAML `yaml:",inline"`
			LLMStage        `yaml:",inline"`
			Type            string `yaml:"type"`
		}
		if err := decodeStrict(node, &v); err != nil {
			return err
		}
		s.init(StageLLM, v.stageCommonYAML)
		s.LLM = &v.LLMStage
	case StageAgent:
		var v struct {
			stageCommonYAML `yaml:",inline"`
			AgentStage      `yaml:",inline"`
			Type            string `yaml:"type"`
		}
		if err := decodeStrict(node, &v); err != nil {
			return err
		}
		s.init(StageAgent, v.stageCommonYAML)
		s.Agent = &v.AgentStage
	case StageTool:
		var v struct {
			stageCommonYAML `yaml:",inline"`
			ToolStage       `yaml:",inline"`
			Type            string `yaml:"type"`
		}
		if err := decodeStrict(node, &v); err != nil {
			return err
		}
		s.init(StageTool, v.stageCommonYAML)
		s.Tool = &v.ToolStage
	case StageHuman:
		var v struct {
			stageCommonYAML `yaml:",inline"`
			HumanStage      `yaml:",inline"`
			Type            string `yaml:"type"`
		}
		if err := decodeStrict(node, &v); err != nil {
			return err
		}
		s.init(StageHuman, v.stageCommonYAML)
		s.Human = &v.HumanStage
	case StageRouter:
		var v struct {
			stageCommonYAML `yaml:",inline"`
			RouterStage     `yaml:",inline"`
			Type            string `yaml:"type"`
		}
		if err := decodeStrict(node, &v); err != nil {
			return err
		}
		s.init(StageRouter, v.stageCommonYAML)
		s.Router = &v.RouterStage
	default:
		return fmt.Errorf("unknown stage type %q (want one of: llm, agent, tool, human, router)", probe.Type)
	}
	return nil
}

func (s *Stage) init(t StageType, c stageCommonYAML) {
	s.ID = c.ID
	s.Type = t
	s.Retry = c.Retry
	s.OnError = c.OnError
}

// Persona is an agent identity shared by rooms and pipeline agent stages.
type Persona struct {
	Name   string       `yaml:"name"`
	Role   string       `yaml:"role,omitempty"`
	System string       `yaml:"system,omitempty"`
	Model  *ModelConfig `yaml:"model,omitempty"`
	Tools  []string     `yaml:"tools,omitempty"`
}

// RuntimeConfig configures the engine's own LLM usage (the "core loop").
type RuntimeConfig struct {
	Narrator *ModelConfig `yaml:"narrator,omitempty"`
}

// Pipeline is a deterministic, ordered sequence of stages.
type Pipeline struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	Personas    []Persona      `yaml:"personas,omitempty"`
	Stages      []Stage        `yaml:"stages"`
	Runtime     *RuntimeConfig `yaml:"runtime,omitempty"`
}

// RoomSettings tunes chat-room conversation dynamics.
type RoomSettings struct {
	SpeakThreshold        float64 `yaml:"speak_threshold,omitempty"`         // 0..1; 0 = engine default
	MaxSpontaneousReplies int     `yaml:"max_spontaneous_replies,omitempty"` // 0 = engine default
	HistoryWindow         int     `yaml:"history_window,omitempty"`          // messages of transcript an observer sees
}

// Room is a channel of persona agents.
type Room struct {
	Name     string       `yaml:"name"`
	Agents   []Persona    `yaml:"agents"`
	Settings RoomSettings `yaml:"settings,omitempty"`
}

// DocumentKind distinguishes the two YAML files loop loads.
type DocumentKind string

const (
	KindPipeline DocumentKind = "pipeline"
	KindRoom     DocumentKind = "room"
)

// IdentifyKind sniffs a loaded document: pipelines have "stages", rooms have "agents".
func IdentifyKind(doc *yaml.Node) (DocumentKind, bool) {
	if doc.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		switch doc.Content[i].Value {
		case "stages":
			return KindPipeline, true
		case "agents":
			return KindRoom, true
		}
	}
	return "", false
}

// validIdent helps enforce machine-friendly ids/names.
func validIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
