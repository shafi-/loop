package cline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/engine"
	"github.com/shafi-/loop/internal/executor"
)

// TestPipelineAgentStageThroughClineExecutor proves the full wiring:
// pipeline YAML → engine runner → registry → this executor → host process
// (a fake here), with LOOP_NODE/LOOP_CLINE_HOST env overrides honored.
func TestPipelineAgentStageThroughClineExecutor(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "index.mjs")
	script := `#!/bin/sh
read task
echo '{"type":"text","text":"designed "}'
echo '{"type":"text","text":"the thing"}'
echo '{"type":"done","output":"architecture doc ready"}'
`
	if err := os.WriteFile(host, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_NODE", "sh")
	t.Setenv("LOOP_CLINE_HOST", host)
	// Keep the standalone host out of the picture (a real machine may
	// have one): this test proves the script-mode env overrides.
	t.Setenv("LOOP_CLINE_HOST_BIN", filepath.Join(dir, "no-standalone-host"))
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // executor resolves keys from env like providers do

	p, err := config.ParsePipeline([]byte(`
name: agent-demo
personas:
  - name: architect
    role: Architect
    system: You design systems.
    model: {provider: anthropic, model: test-model}
stages:
  - id: spec
    type: tool
    run: echo the-spec
  - id: design
    type: agent
    executor: cline
    persona: architect
    input: "design ${stages.spec.output}"
    tools: [read_file]
`))
	if err != nil {
		t.Fatal(err)
	}

	reg := executor.NewRegistry()
	reg.Register(New())
	r := engine.Runner{Pipeline: p, Source: []byte("x"), Executors: reg, RunsDir: dir}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("run failed at %s: %v", res.FailedStage, res.Err)
	}
	snap, err := os.ReadFile(filepath.Join(dir, res.RunID, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap), "architecture doc ready") {
		t.Errorf("executor output missing from context:\n%s", snap)
	}
}
