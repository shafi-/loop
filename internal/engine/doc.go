// Package engine executes pipelines: it owns the run context (stage I/O
// and template interpolation), runs stages in order with router branching,
// enforces retry/on_error semantics, and persists every run to
// .loop/runs/<id>/ for auditing and --resume.
package engine
