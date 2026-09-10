package engine

import (
	"strings"
	"testing"

	"github.com/nerddevsltd/loop/internal/config"
)

func TestInterpolateBothSyntaxes(t *testing.T) {
	c := NewContext(map[string]any{"idea": "oauth login"})
	c.SetOutput("requirements", "output", "REQ DOC")
	c.SetOutput("approval", "answer", "yes")
	c.outputs["req_alias"] = "REQ DOC"

	cases := []struct{ in, want string }{
		{"idea: {{ vars.idea }}", "idea: oauth login"},
		{"idea: ${vars.idea}", "idea: oauth login"},
		{"doc: ${stages.requirements.output}", "doc: REQ DOC"},
		{"doc: ${stages.requirements}", "doc: REQ DOC"},      // field defaults to output
		{"answer: ${stages.approval.answer}", "answer: yes"}, // dotted alias key
		{"alias: ${outputs.req_alias}", "alias: REQ DOC"},
		{"no refs here", "no refs here"},
		{"mixed ${vars.idea} and {{ vars.idea }}", "mixed oauth login and oauth login"},
	}
	for _, tc := range cases {
		got, err := c.Interpolate(tc.in)
		if err != nil {
			t.Errorf("Interpolate(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInterpolateMissingReferencesFailLoudly(t *testing.T) {
	c := NewContext(map[string]any{})
	c.SetOutput("a", "output", "x")
	for _, bad := range []string{
		"{{ vars.nope }}",
		"${stages.nope.output}",
		"${stages.a.answer}", // exists but no answer field
		"${outputs.nope}",
		"${nope.thing}",
	} {
		if _, err := c.Interpolate(bad); err == nil {
			t.Errorf("Interpolate(%q) should fail", bad)
		}
	}
}

func TestContextSnapshotRoundTrip(t *testing.T) {
	c := NewContext(map[string]any{"k": "v", "n": 42})
	c.SetOutput("s1", "output", "out1")
	c.SetOutput("h", "answer", "yes")
	c.outputs["alias.name"] = "aliased"

	restored := Restore(c.Snapshot())
	got, err := restored.Interpolate("${vars.k} ${vars.n} ${stages.s1} ${stages.h.answer} ${outputs.alias.name}")
	if err != nil {
		t.Fatal(err)
	}
	want := "v 42 out1 yes aliased"
	if got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

func TestEvalComparison(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"yes == 'yes'", true},
		{`"yes" == "yes"`, true},
		{"yes == 'no'", false},
		{"yes != 'no'", true},
		{" 'yes'  == 'yes' ", true}, // interpolation can leave padding
	}
	for _, tc := range cases {
		got, err := evalComparison(tc.expr)
		if err != nil {
			t.Errorf("evalComparison(%q) error: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("evalComparison(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
	if _, err := evalComparison("something fancier"); err == nil {
		t.Error("non-comparison expressions must be rejected, not guessed")
	}
}

func TestToolCapturesStdoutAndEnv(t *testing.T) {
	c := NewContext(map[string]any{"who": "world"})
	c.SetOutput("upstream", "output", "seed")
	d := &stageDeps{}
	s := &config.Stage{ID: "t", Type: config.StageTool, Tool: &config.ToolStage{
		Run:   `echo "hello $TARGET"; cat`,
		Env:   map[string]string{"TARGET": "${vars.who}"},
		Input: "stdin:${stages.upstream.output}",
	}}
	out, err := runToolStage(t.Context(), s, c, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "hello world\nstdin:seed") {
		t.Errorf("tool output = %q", out.Output)
	}
}

func TestToolFailureCarriesStderr(t *testing.T) {
	c := NewContext(map[string]any{})
	s := &config.Stage{ID: "t", Type: config.StageTool, Tool: &config.ToolStage{
		Run: `echo "boom detail" >&2; exit 3`,
	}}
	_, err := runToolStage(t.Context(), s, c, &stageDeps{})
	if err == nil || !strings.Contains(err.Error(), "boom detail") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("tool failure = %v", err)
	}
}

func TestRouterPicksFirstMatchingRule(t *testing.T) {
	c := NewContext(map[string]any{})
	c.SetOutput("approval", "answer", "no")
	s := &config.Stage{ID: "gate", Type: config.StageRouter, Router: &config.RouterStage{When: []config.RouteRule{
		{If: "${stages.approval.answer} == 'yes'", Next: "build"},
		{If: "${stages.approval.answer} != 'maybe'", Next: "rework"},
		{Next: "archive"},
	}}}
	out, err := runRouterStage(t.Context(), s, c, &stageDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Next != "rework" || out.RuleIndex != 1 {
		t.Errorf("router picked %q (rule %d)", out.Next, out.RuleIndex)
	}
}

func TestRouterNoMatchWithoutDefault(t *testing.T) {
	c := NewContext(map[string]any{})
	c.SetOutput("approval", "answer", "no")
	s := &config.Stage{ID: "gate", Type: config.StageRouter, Router: &config.RouterStage{When: []config.RouteRule{
		{If: "${stages.approval.answer} == 'yes'", Next: "build"},
	}}}
	if _, err := runRouterStage(t.Context(), s, c, &stageDeps{}); err == nil {
		t.Error("router without a match or default must fail")
	}
}
