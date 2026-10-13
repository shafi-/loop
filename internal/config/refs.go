package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// TemplateRefRe matches both reference syntaxes, {{ path }} and ${path}.
// It lives here as the single source of truth: the engine's interpolator
// (engine/context.go) resolves exactly what this matches, and validation
// checks exactly what the engine will try to resolve — neither side can
// drift from the other.
var TemplateRefRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}|\$\{([^{}]+)\}`)

// roomOnlyVars are injected by the room hand-off (cli/roomrun.go) into
// every pipeline run it starts, so templates may reference them without
// a vars: declaration.
var roomOnlyVars = []string{"room", "room_context"}

// stageTemplateField is one templated field: its YAML path relative to
// the stage, and the raw text.
type stageTemplateField struct {
	path string
	text string
}

// stageTemplates lists a stage's templated fields — exactly the strings
// the engine interpolates (llm.prompt, agent.input, tool.run/input/env,
// human.prompt, router.when[].if). A field the engine does not
// interpolate (llm.system) is deliberately absent: there a ${...} is
// literal text, not a reference.
func stageTemplates(s *Stage) []stageTemplateField {
	var fields []stageTemplateField
	switch s.Type {
	case StageLLM:
		fields = append(fields, stageTemplateField{"prompt", s.LLM.Prompt})
	case StageAgent:
		fields = append(fields, stageTemplateField{"input", s.Agent.Input})
	case StageTool:
		fields = append(fields,
			stageTemplateField{"run", s.Tool.Run},
			stageTemplateField{"input", s.Tool.Input},
		)
		// Env keys sorted: validation output must not depend on map order.
		keys := make([]string, 0, len(s.Tool.Env))
		for k := range s.Tool.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fields = append(fields, stageTemplateField{"env[" + k + "]", s.Tool.Env[k]})
		}
	case StageHuman:
		fields = append(fields, stageTemplateField{"prompt", s.Human.Prompt})
	case StageRouter:
		for j := range s.Router.When {
			fields = append(fields, stageTemplateField{fmt.Sprintf("when[%d].if", j), s.Router.When[j].If})
		}
	}
	return fields
}

// extractRefs returns every reference path in s, in order.
func extractRefs(s string) []string {
	var refs []string
	for _, m := range TemplateRefRe.FindAllStringSubmatch(s, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		refs = append(refs, raw)
	}
	return refs
}

// declaredOutput returns a stage's explicit output name as the engine's
// stageAlias sees it: an llm/agent/tool stage's `output:` when set. A
// human stage stores no alias — its answer reads back only as
// stages.<id>.answer.
func declaredOutput(s *Stage) string {
	switch s.Type {
	case StageLLM:
		return s.LLM.Output
	case StageAgent:
		return s.Agent.Output
	case StageTool:
		return s.Tool.Output
	}
	return ""
}

// validateTemplateRefs checks every template reference against the
// pipeline's own declarations — stage ids, output aliases, declared vars
// — plus the one ordering rule execution enforces: a stage can only read
// what has already run. Creation time is when this belongs: a bad
// reference is a static defect the engine would hit mid-run (resume
// cannot fix it — the text never changes), so it is cheaper rejected
// here, at the exact YAML path, with the nearest real name in the error.
func (p *Pipeline) validateTemplateRefs(err func(path, format string, args ...any), ids map[string]int) {
	aliases := make(map[string]int) // output alias name -> first producing stage index
	for i := range p.Stages {
		if name := declaredOutput(&p.Stages[i]); name != "" && name != p.Stages[i].ID {
			// First declaration wins: a revision loop re-declares the same
			// alias on a later revise stage to overwrite it, and consumers
			// read against the original producer.
			if _, dup := aliases[name]; !dup {
				aliases[name] = i
			}
		}
	}
	stageIDs := make([]string, 0, len(ids))
	for i := range p.Stages {
		if _, ok := ids[p.Stages[i].ID]; ok {
			stageIDs = append(stageIDs, p.Stages[i].ID)
		}
	}
	aliasNames := sortedKeys(aliases)
	varNames := make([]string, 0, len(p.Vars)+len(roomOnlyVars))
	for k := range p.Vars {
		varNames = append(varNames, k)
	}
	varNames = append(varNames, roomOnlyVars...)
	sort.Strings(varNames)

	check := func(fieldPath, ref string, i int) {
		parts := strings.Split(strings.TrimSpace(ref), ".")
		invalid := func() {
			err(fieldPath, "invalid reference %q (want vars.<name>, stages.<id>[.field], or outputs.<name>)", ref)
		}
		switch parts[0] {
		case "vars":
			if len(parts) != 2 {
				invalid()
				return
			}
			if _, ok := p.Vars[parts[1]]; ok {
				return
			}
			for _, injected := range roomOnlyVars {
				if parts[1] == injected {
					return
				}
			}
			err(fieldPath, "references undeclared var %q%s — declare it under vars: or pass --var", parts[1], SuggestNearest(parts[1], varNames))
		case "stages":
			if len(parts) < 2 || len(parts) > 3 {
				invalid()
				return
			}
			id := parts[1]
			j, ok := ids[id]
			if !ok {
				err(fieldPath, "references %s — no stage with this id%s", ref, SuggestNearest(id, stageIDs))
				return
			}
			if j >= i {
				err(fieldPath, "references %s — stage %q has not run yet (only stages listed before this one are readable)", ref, id)
				return
			}
			if len(parts) == 3 {
				target := &p.Stages[j]
				switch parts[2] {
				case "output", "status":
				case "answer":
					if target.Type != StageHuman {
						err(fieldPath, "stage %q has no %q — answers exist only on human stages", id, parts[2])
					}
				case "intent":
					if target.Type != StageHuman || !target.Human.Gate {
						err(fieldPath, "stage %q has no %q — intent exists only on a human gate (gate: true)", id, parts[2])
					}
				default:
					err(fieldPath, "stage %q has no %q (a stage exposes output, answer, intent, status)", id, parts[2])
				}
			}
		case "outputs":
			if len(parts) < 2 {
				invalid()
				return
			}
			// Alias names may themselves contain dots ("approval.answer").
			name := strings.Join(parts[1:], ".")
			j, ok := aliases[name]
			if !ok {
				err(fieldPath, "references outputs.%s — no stage declares that output alias%s", name, SuggestNearest(name, aliasNames))
				return
			}
			if j >= i {
				err(fieldPath, "references outputs.%s — produced by stage %q, which runs later (only earlier aliases are readable)", name, p.Stages[j].ID)
			}
		default:
			invalid()
		}
	}

	for i := range p.Stages {
		for _, f := range stageTemplates(&p.Stages[i]) {
			for _, ref := range extractRefs(f.text) {
				check(fmt.Sprintf("stages[%d].%s", i, f.path), ref, i)
			}
		}
	}
}

// SuggestNearest renders ` (did you mean "x"?)` naming the candidate
// closest to bad — built for the hyphen/underscore class of typo (id
// "architect-signoff" referenced as "architect_signoff") — or "" when
// nothing is within edit distance 2.
func SuggestNearest(bad string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if c == bad {
			continue
		}
		if d := editDistance(bad, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

// editDistance is Levenshtein: insertion, deletion, substitution.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func minInt(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
