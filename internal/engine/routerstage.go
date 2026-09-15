package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/shafi-/loop/internal/config"
)

// runRouterStage evaluates rules in order; the first true `if` wins, and
// the final rule without `if` is the default arm. Expressions interpolate
// first, then support `==` / `!=` string comparisons — deliberately tiny;
// a real expression language is a conscious future addition.
func runRouterStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	for i := range s.Router.When {
		rule := &s.Router.When[i]
		if rule.If == "" {
			return &stageOutcome{Next: rule.Next, RuleIndex: i}, nil
		}
		expr, err := c.Interpolate(rule.If)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		match, err := evalComparison(expr)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%q): %w", i, expr, err)
		}
		if match {
			return &stageOutcome{Next: rule.Next, RuleIndex: i}, nil
		}
	}
	return nil, fmt.Errorf("no rule matched and there is no default arm")
}

// evalComparison evaluates the two expressions the router supports after
// interpolation: `A == B` and `A != B`. Operands may be quoted with single
// or double quotes; surrounding whitespace is insignificant.
func evalComparison(expr string) (bool, error) {
	for _, op := range []string{"==", "!="} {
		if i := strings.Index(expr, op); i >= 0 {
			a, err := unquote(strings.TrimSpace(expr[:i]))
			if err != nil {
				return false, err
			}
			b, err := unquote(strings.TrimSpace(expr[i+2:]))
			if err != nil {
				return false, err
			}
			if op == "==" {
				return a == b, nil
			}
			return a != b, nil
		}
	}
	return false, fmt.Errorf("unsupported expression (want `A == B` or `A != B` after interpolation)")
}

// unquote strips one layer of surrounding quotes and rejects internal newlines.
func unquote(s string) (string, error) {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			s = s[1 : len(s)-1]
		}
	}
	if strings.ContainsAny(s, "\n\r") {
		return "", fmt.Errorf("multi-line operand — wrap the reference in quotes inside the expression")
	}
	return s, nil
}
