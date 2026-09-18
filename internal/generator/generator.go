// Package generator turns a natural-language description into a
// validated loop document — a pipeline or a chat room. The LLM drafts;
// config's validator decides; validation errors feed back for repair.
// The validator — not the provider's schema enforcement — is the source
// of truth, which keeps the generator identical for every provider
// including ones without strict JSON modes.
package generator

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

const defaultMaxRepairs = 2

// Generator produces documents from descriptions using one provider.
type Generator struct {
	Provider     llm.Provider
	ProviderName string // "anthropic" | "openai": emitted model blocks target this family
	Model        string
	MaxRepairs   int                              // repair passes after the first draft; 0 = default
	Logf         func(format string, args ...any) // progress lines, may be nil
}

// Result carries the validated document and its YAML rendering; exactly
// one of Pipeline/Room is set, per the kind that was drafted.
type Result struct {
	Pipeline *config.Pipeline
	Room     *config.Room
	YAML     []byte
}

// Generate runs draft → validate → repair until the pipeline passes or
// the repair budget is spent.
func (g *Generator) Generate(ctx context.Context, description string) (*Result, error) {
	parsed, y, err := g.draft(ctx, description, draftKind{
		noun:   "pipeline",
		system: systemPrompt,
		prompt: func() string { return buildPrompt(description, g.ProviderName) },
		parse:  func(doc []byte) (any, error) { return config.ParsePipeline(doc) },
	})
	if err != nil {
		return nil, err
	}
	return &Result{Pipeline: parsed.(*config.Pipeline), YAML: y}, nil
}

// GenerateRoom runs the same draft → validate → repair loop for chat
// rooms: the gate is config.ParseRoom and the contract teaches the room
// schema — agents with distinct seats, no model blocks (rooms follow the
// environment's provider family per the grand rule). availablePersona
// lists the workspace's library personas; the draft references those by
// name instead of reinventing them.
func (g *Generator) GenerateRoom(ctx context.Context, description string, availablePersonas []string) (*Result, error) {
	parsed, y, err := g.draft(ctx, description, draftKind{
		noun:   "room",
		system: roomSystemPrompt,
		prompt: func() string { return buildRoomPrompt(description, availablePersonas) },
		parse:  func(doc []byte) (any, error) { return config.ParseRoom(doc) },
	})
	if err != nil {
		return nil, err
	}
	return &Result{Room: parsed.(*config.Room), YAML: y}, nil
}

// draftKind is one generator target: a system prompt, a prompt builder,
// and the strict validator that gates every attempt.
type draftKind struct {
	noun   string
	system string
	prompt func() string
	parse  func(doc []byte) (any, error)
}

func (g *Generator) draft(ctx context.Context, description string, k draftKind) (any, []byte, error) {
	maxRepairs := g.MaxRepairs
	if maxRepairs == 0 {
		maxRepairs = defaultMaxRepairs
	}
	logf := g.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	prompt := k.prompt()
	var lastErr error
	for attempt := 0; attempt <= maxRepairs; attempt++ {
		if attempt > 0 {
			logf("repair pass %d/%d...", attempt, maxRepairs)
			prompt = repairPrompt(prompt, lastErr)
		}
		resp, err := g.Provider.Complete(ctx, llm.Request{
			Model:    g.Model,
			System:   k.system,
			Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("generation failed: %w", err)
		}

		doc, err := extractJSON(resp.Text)
		if err != nil {
			lastErr = err
			continue
		}
		parsed, err := k.parse(doc)
		if err != nil {
			lastErr = err
			continue
		}
		y, err := jsonToYAML(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("converting to YAML: %w", err)
		}
		return parsed, y, nil
	}
	return nil, nil, fmt.Errorf("LLM could not produce a valid %s after %d repair pass(es):\n%v", k.noun, maxRepairs, lastErr)
}

const systemPrompt = `You are a pipeline compiler for "loop", a deterministic agentic
harness. The user describes a process; you emit exactly one JSON object
describing a loop pipeline. Output ONLY the JSON object — no prose, no
markdown fences. Use double-quoted JSON strings; prompt fields may
contain \n escapes for multi-line text.`

const roomSystemPrompt = `You are a room compiler for "loop", a deterministic agentic
harness. The user describes a team and what it is for; you emit exactly
one JSON object describing a loop chat room. Output ONLY the JSON
object — no prose, no markdown fences. Use double-quoted JSON strings;
system fields may contain \n escapes for multi-line text.`

// buildPrompt composes the contract reference, the worked example, and the
// user's description into one drafting prompt. A non-empty providerName
// pins every emitted model block to that API family — without it, models
// tend to copy the worked example's providers, which may not be what the
// user has credentials for.
func buildPrompt(description, providerName string) string {
	var b strings.Builder
	b.WriteString(pipelineContract)
	if providerName != "" {
		b.WriteString("\n\nPROVIDER CONSTRAINT: every \"model\" object MUST use \"provider\": \"" +
			providerName + "\" — that is the provider configured in this environment. " +
			"Never emit any other provider.")
	}
	b.WriteString("\n\n--- EXAMPLE (YAML form; emit the same structure as JSON) ---\n")
	b.WriteString(examplePipeline)
	b.WriteString("\n--- DESCRIPTION ---\n")
	b.WriteString(strings.TrimSpace(description))
	b.WriteString("\n\nRemember: one JSON object only. Every stage needs a unique,\n" +
		"lowercase-kebab `id`; a router needs a final default rule without \"if\";\n" +
		"only providers \"anthropic\" and \"openai\" exist; only tools read_file,\n" +
		"write_file, run_command exist.")
	return b.String()
}

// buildRoomPrompt composes the room contract, the worked example, and the
// user's description. Rooms never emit model blocks — every agent follows
// the environment's configured family. availablePersonas lists the
// workspace's library personas: the draft references those by name
// instead of reinventing seats that already exist.
func buildRoomPrompt(description string, availablePersonas []string) string {
	var b strings.Builder
	b.WriteString(roomContract)
	if len(availablePersonas) > 0 {
		b.WriteString("\n\nAVAILABLE PERSONAS (the workspace's persona library): " +
			strings.Join(availablePersonas, ", ") +
			"\nWhen the team should include one of these, use an agent entry with " +
			"only \"persona\": \"<name>\" (plus optional \"tools\") — do not restate " +
			"its name, role, or system. Mix references and concrete agents freely.")
	}
	b.WriteString("\n\n--- EXAMPLE (YAML form; emit the same structure as JSON) ---\n")
	b.WriteString(exampleRoom)
	b.WriteString("\n--- DESCRIPTION ---\n")
	b.WriteString(strings.TrimSpace(description))
	b.WriteString("\n\nRemember: one JSON object only. Every concrete agent needs a unique,\n" +
		"lowercase-kebab `name` and a `system` prompt written in second person\n" +
		"that says when to speak and when to stay silent. Only tools read_file,\n" +
		"write_file, run_command exist. Never emit \"model\" objects. Include a\n" +
		"\"pipelines\" section only if the description names a pipeline file.")
	return b.String()
}

// repairPrompt appends the validation failure so the next draft fixes it.
func repairPrompt(prev string, err error) string {
	return prev + "\n\n--- YOUR PREVIOUS OUTPUT FAILED VALIDATION ---\n" +
		err.Error() +
		"\nEmit a corrected single JSON object. Fix every listed problem."
}

// extractJSON pulls the JSON object out of a model response, tolerating
// markdown fences and leading prose (models drift; the validator is the
// real gate, this just gets us to parseable bytes).
func extractJSON(text string) ([]byte, error) {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[i:]
	}
	if i := strings.LastIndex(s, "}"); i >= 0 {
		s = s[:i+1]
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("response contained no JSON object (%d chars)", len(text))
	}
	return []byte(s), nil
}

// jsonToYAML re-renders the validated JSON document as YAML. Going through
// yaml.Node preserves the model's key order and costs nothing in fidelity:
// the bytes were already accepted by the strict config decoder.
func jsonToYAML(doc []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(doc, &node); err != nil {
		return nil, err
	}
	// JSON input decodes with flow/double-quote styles set; without
	// clearing them the "YAML" output would be JSON in disguise. The
	// encoder re-adds minimal quoting where the content requires it.
	clearStyles(node.Content[0])
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node.Content[0]); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clearStyles(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		clearStyles(c)
	}
}
