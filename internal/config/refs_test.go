package config

import (
	"strings"
	"testing"
)

// kebabPipeline is the corrected shape of the pipeline that motivated
// reference validation: ids carry hyphens, routers branch on gate
// intents, a revise stage reads the gate's answer, a named output alias
// feeds a later stage.
const kebabPipeline = `
name: req-to-plan
vars:
  idea: ""
stages:
  - id: requirements
    type: llm
    prompt: "Draft requirements for: {{ vars.idea }}"
    output: requirements_md
  - id: architect-signoff
    type: human
    gate: true
    prompt: "Sign off on these requirements? ${outputs.requirements_md}"
  - id: architect-router
    type: router
    when:
      - if: "${stages.architect-signoff.intent} == 'yes'"
        next: plan
      - next: revise
  - id: revise
    type: llm
    prompt: "Address these issues: ${stages.architect-signoff.answer}"
  - id: plan
    type: llm
    prompt: "Plan for: ${stages.requirements.output}"
    output: plan_md
  - id: approved
    type: tool
    terminal: true
    run: echo done
`

func TestValidateTemplateRefsAcceptsKebabPipeline(t *testing.T) {
	if _, err := ParsePipeline([]byte(kebabPipeline)); err != nil {
		t.Fatalf("correct references rejected:\n%v", err)
	}
}

func TestValidateTemplateRefs(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string // substrings required in the error; nil = must parse clean
	}{
		{
			name: "underscore where the id has a hyphen — the failure that motivated this",
			doc: `
name: req-to-plan
stages:
  - id: requirements
    type: llm
    prompt: "Draft requirements"
  - id: architect-signoff
    type: human
    gate: true
    prompt: "Sign off?"
  - id: architect-router
    type: router
    when:
      - if: "${stages.architect_signoff.intent} == 'yes'"
        next: plan
      - next: revise
  - id: revise
    type: llm
    prompt: "Fix"
  - id: plan
    type: llm
    prompt: "Plan"
`,
			want: []string{
				"stages[2].when[0].if",
				`references stages.architect_signoff.intent — no stage with this id`,
				`did you mean "architect-signoff"`,
			},
		},
		{
			name: "underscore in an answer reference",
			doc: `
name: req-to-plan
stages:
  - id: architect-signoff
    type: human
    gate: true
    prompt: "Sign off?"
  - id: revise
    type: llm
    prompt: "Address these issues: ${stages.architect_signoff.answer}"
`,
			want: []string{
				"stages[1].prompt",
				`did you mean "architect-signoff"`,
			},
		},
		{
			name: "forward reference",
			doc: `
name: forward
stages:
  - id: first
    type: llm
    prompt: "Use ${stages.later.output}"
  - id: later
    type: llm
    prompt: "Later"
`,
			want: []string{
				"stages[0].prompt",
				`stage "later" has not run yet (only stages listed before this one are readable)`,
			},
		},
		{
			name: "answer of a non-human stage",
			doc: `
name: answer-on-llm
stages:
  - id: requirements
    type: llm
    prompt: "Draft"
  - id: ask
    type: human
    prompt: "See ${stages.requirements.answer}"
`,
			want: []string{
				`stage "requirements" has no "answer" — answers exist only on human stages`,
			},
		},
		{
			name: "intent without gate",
			doc: `
name: intent-no-gate
stages:
  - id: ask
    type: human
    prompt: "Ok?"
  - id: route
    type: router
    when:
      - if: "${stages.ask.intent} == 'yes'"
        next: done
      - next: ask
  - id: done
    type: tool
    terminal: true
    run: echo done
`,
			want: []string{
				`stage "ask" has no "intent" — intent exists only on a human gate (gate: true)`,
			},
		},
		{
			name: "unknown field on a stage",
			doc: `
name: unknown-field
stages:
  - id: first
    type: llm
    prompt: "Draft"
  - id: second
    type: llm
    prompt: "Use ${stages.first.snippet}"
`,
			want: []string{
				`stage "first" has no "snippet" (a stage exposes output, answer, intent, status)`,
			},
		},
		{
			name: "unknown output alias with a near typo",
			doc: `
name: alias-typo
stages:
  - id: requirements
    type: llm
    prompt: "Draft"
    output: requirements_md
  - id: ask
    type: human
    gate: true
    prompt: "Sign off? ${outputs.requirments_md}"
`,
			want: []string{
				`references outputs.requirments_md — no stage declares that output alias`,
				`did you mean "requirements_md"`,
			},
		},
		{
			name: "output alias produced later",
			doc: `
name: alias-forward
stages:
  - id: first
    type: llm
    prompt: "Use ${outputs.plan_md}"
  - id: plan
    type: llm
    prompt: "Plan"
    output: plan_md
`,
			want: []string{
				`produced by stage "plan", which runs later`,
			},
		},
		{
			name: "undeclared var with a near typo",
			doc: `
name: var-typo
vars:
  idea: ""
stages:
  - id: first
    type: llm
    prompt: "Idea: {{ vars.idae }}"
`,
			want: []string{
				`references undeclared var "idae"`,
				`did you mean "idea"`,
			},
		},
		{
			name: "room-injected vars need no declaration",
			doc: `
name: room-vars
stages:
  - id: first
    type: llm
    prompt: "Room ${vars.room}, context ${vars.room_context}"
`,
			want: nil,
		},
		{
			name: "reference in a tool env value",
			doc: `
name: env-ref
stages:
  - id: first
    type: tool
    run: printenv FOO
    env:
      FOO: "${stages.nope.output}"
`,
			want: []string{
				"stages[0].env[FOO]",
				`no stage with this id`,
			},
		},
		{
			name: "reference in an agent input",
			doc: `
name: agent-ref
personas:
  - name: worker
    role: Worker
    system: You work.
stages:
  - id: first
    type: agent
    persona: worker
    input: "Use ${stages.missing.output}"
`,
			want: []string{
				"stages[0].input",
				`no stage with this id`,
			},
		},
		{
			name: "unknown namespace",
			doc: `
name: bad-namespace
stages:
  - id: first
    type: llm
    prompt: "Token: ${secrets.token}"
`,
			want: []string{
				`invalid reference "secrets.token"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePipeline([]byte(tt.doc))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("expected clean parse, got:\n%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected validation errors, got none")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestSuggestNearest(t *testing.T) {
	candidates := []string{"requirements", "architect-signoff", "plan"}
	if got := SuggestNearest("architect_signoff", candidates); got != ` (did you mean "architect-signoff"?)` {
		t.Errorf("hyphen/underscore typo: got %q", got)
	}
	if got := SuggestNearest("entirely-different", candidates); got != "" {
		t.Errorf("far typo should get no suggestion, got %q", got)
	}
	if got := SuggestNearest("architect-signoff", candidates); got != "" {
		t.Errorf("exact id should get no suggestion, got %q", got)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"", "abc", 3},
		{"abc", "", 3},
		{"architect_signoff", "architect-signoff", 1},
		{"idea", "idae", 2},
		{"same", "same", 0},
	} {
		if got := editDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
