package engine

import (
	"fmt"
	"regexp"
	"strings"
)

// Context is the run's data spine: pipeline vars plus the accumulated
// outputs of executed stages. Stages read from it via templates and write
// canonical entries into it.
//
// Layout (mirrors the reference syntax used in pipeline YAML):
//
//	vars.<name>              pipeline inputs
//	stages.<id>.output       every stage's result text
//	stages.<id>.answer       a human stage's raw reply (output mirrors it)
//	outputs.<name>           alias when a stage sets an explicit output name
type Context struct {
	vars    map[string]any
	stages  map[string]map[string]string
	outputs map[string]string
	// lastClipped remembers which references the most recent
	// InterpolatePrompt clipped, for the runner to log. Stages run
	// sequentially on one goroutine, so no lock is needed.
	lastClipped []string
}

func NewContext(vars map[string]any) *Context {
	v := map[string]any{}
	for k, val := range vars {
		v[k] = val
	}
	return &Context{vars: v, stages: map[string]map[string]string{}, outputs: map[string]string{}}
}

// SetOutput records a stage's result under its canonical path, plus the
// human answer field and the explicit alias when configured.
func (c *Context) SetOutput(stageID, field, value string) {
	m := c.stages[stageID]
	if m == nil {
		m = map[string]string{}
		c.stages[stageID] = m
	}
	m[field] = value
}

// Snapshot returns a deep copy for persistence (context.json).
func (c *Context) Snapshot() map[string]any {
	out := map[string]any{"vars": map[string]any{}, "stages": map[string]any{}, "outputs": map[string]any{}}
	for k, v := range c.vars {
		out["vars"].(map[string]any)[k] = v
	}
	for id, fields := range c.stages {
		f := map[string]any{}
		for k, v := range fields {
			f[k] = v
		}
		out["stages"].(map[string]any)[id] = f
	}
	for k, v := range c.outputs {
		out["outputs"].(map[string]any)[k] = v
	}
	return out
}

// Restore reloads a snapshot produced by Snapshot (resume path).
func Restore(data map[string]any) *Context {
	c := NewContext(nil)
	if vars, ok := data["vars"].(map[string]any); ok {
		for k, v := range vars {
			c.vars[k] = v
		}
	}
	if stages, ok := data["stages"].(map[string]any); ok {
		for id, fields := range stages {
			m, ok := fields.(map[string]any)
			if !ok {
				continue
			}
			for f, v := range m {
				c.SetOutput(id, f, fmt.Sprint(v))
			}
		}
	}
	if outputs, ok := data["outputs"].(map[string]any); ok {
		for k, v := range outputs {
			c.outputs[k] = fmt.Sprint(v)
		}
	}
	return c
}

// resolve walks a dotted reference ("vars.idea", "stages.a.output") and
// returns its value. The last path element of a stage reference defaults
// to "output" when omitted.
func (c *Context) resolve(path string) (string, error) {
	path = strings.TrimSpace(path)
	parts := strings.Split(path, ".")
	switch parts[0] {
	case "vars":
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid reference %q (want vars.<name>)", path)
		}
		v, ok := c.vars[parts[1]]
		if !ok {
			return "", fmt.Errorf("unknown vars.%s", parts[1])
		}
		return fmt.Sprint(v), nil
	case "outputs":
		// Alias names may themselves contain dots (e.g. "approval.answer"),
		// so everything after "outputs" is the key.
		if len(parts) < 2 {
			return "", fmt.Errorf("invalid reference %q (want outputs.<name>)", path)
		}
		key := strings.Join(parts[1:], ".")
		v, ok := c.outputs[key]
		if !ok {
			return "", fmt.Errorf("unknown outputs.%s (not set by any stage yet)", key)
		}
		return v, nil
	case "stages":
		if len(parts) < 2 || len(parts) > 3 {
			return "", fmt.Errorf("invalid reference %q (want stages.<id>[.output|.answer])", path)
		}
		id := parts[1]
		field := "output"
		if len(parts) == 3 {
			field = parts[2]
		}
		m, ok := c.stages[id]
		if !ok {
			return "", fmt.Errorf("unknown stages.%s (stage has not run)", id)
		}
		v, ok := m[field]
		if !ok {
			return "", fmt.Errorf("stages.%s has no %q", id, field)
		}
		return v, nil
	default:
		return "", fmt.Errorf("reference %q must start with vars., stages., or outputs.", path)
	}
}

// refRe matches both reference syntaxes: {{ path }} and ${path}.
var refRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}|\$\{([^{}]+)\}`)

// Interpolate resolves every reference in s against the context. Unknown
// references are hard errors — silent empty strings would corrupt a
// deterministic run.
func (c *Context) Interpolate(s string) (string, error) {
	return c.interpolate(s, 0)
}

// MaxPromptValue bounds one interpolated value entering a model prompt
// or agent instruction (InterpolatePrompt). Tool commands, env values,
// and router expressions use plain Interpolate and stay unclipped —
// commands may legitimately carry bulk data, and routers compare exact
// strings.
const MaxPromptValue = 64 << 10

const clippedValueMarker = "\n[… value clipped at %d bytes — the full text is in the run's context.json …]\n"

// InterpolatePrompt is Interpolate for text a model will read: any
// single resolved value larger than MaxPromptValue renders clipped with
// a marker saying so (the model knows, the run log can be checked).
// The context itself keeps full values — audit and resume are exact.
func (c *Context) InterpolatePrompt(s string) (string, error) {
	return c.interpolate(s, MaxPromptValue)
}

func (c *Context) interpolate(s string, maxValue int) (string, error) {
	var perr error
	var clipped []string
	out := refRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := refRe.FindStringSubmatch(m)
		raw := groups[1]
		if raw == "" {
			raw = groups[2]
		}
		v, err := c.resolve(raw)
		if err != nil {
			perr = err
			return m
		}
		if maxValue > 0 && len(v) > maxValue {
			clipped = append(clipped, raw)
			return v[:maxValue] + fmt.Sprintf(clippedValueMarker, maxValue)
		}
		return v
	})
	if perr != nil {
		return "", perr
	}
	c.lastClipped = clipped
	return out, nil
}

// TakeClipped returns (and clears) the references clipped by the most
// recent InterpolatePrompt call — the caller logs them as events so a
// clipped prompt is visible, never silent.
func (c *Context) TakeClipped() []string {
	clipped := c.lastClipped
	c.lastClipped = nil
	return clipped
}
