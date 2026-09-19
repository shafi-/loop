package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `loop run` must resolve persona-library references: the shipped starter
// pipelines declare personas as `persona: engineer` (resolved from the
// project or global library), and agent stages look them up by name — an
// unresolved reference fails every agent stage with "persona not found in
// pipeline".
func TestRunResolvesPersonaReferences(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the global persona library

	const engineer = "name: engineer\nsystem: You build things.\n"
	const pipeline = `name: ref-check
personas:
  - persona: engineer
stages:
  - id: greet
    type: tool
    run: echo done
`
	writeFile := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("project persona library resolves", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, "personas")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "engineer.yaml"), engineer)
		file := filepath.Join(ws, "ref-check.yaml")
		writeFile(t, file, pipeline)
		if _, _, err := runCLI(t, "", "run", file, "--quiet", "--runs-dir", filepath.Join(ws, ".loop", "runs")); err != nil {
			t.Fatalf("run with the persona available: %v", err)
		}
	})

	t.Run("global persona library resolves", func(t *testing.T) {
		ws := t.TempDir()
		gdir := filepath.Join(t.TempDir(), ".loop", "personas") // HOME is this test's temp dir
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gdir, "engineer.yaml"), []byte(engineer), 0o644); err != nil {
			t.Fatal(err)
		}
		home := filepath.Dir(filepath.Dir(gdir))
		t.Setenv("HOME", home)
		file := filepath.Join(ws, "ref-check.yaml")
		writeFile(t, file, pipeline)
		if _, _, err := runCLI(t, "", "run", file, "--quiet", "--runs-dir", filepath.Join(ws, ".loop", "runs")); err != nil {
			t.Fatalf("run with the persona in the global library: %v", err)
		}
	})

	t.Run("missing persona fails at load", func(t *testing.T) {
		ws := t.TempDir()
		file := filepath.Join(ws, "ref-check.yaml")
		writeFile(t, file, pipeline)
		_, _, err := runCLI(t, "", "run", file, "--quiet", "--runs-dir", filepath.Join(ws, ".loop", "runs"))
		if err == nil || !strings.Contains(err.Error(), `unknown persona "engineer"`) {
			t.Fatalf("err = %v, want unknown persona", err)
		}
	})
}
