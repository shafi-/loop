package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// resolveFile accepts room aliases AND workspace pipeline paths; the
// run's label is the alias, or the path's base without extension.
func TestResolveFileAliasOrPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".loop/pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".loop/pipelines", "adhoc.yaml"), []byte("name: adhoc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &roomRunSession{
		roomPath:  filepath.Join(dir, ".loop/rooms", "demo.yaml"),
		pipelines: []config.RoomPipeline{{Name: "implement", File: "../pipelines/implement.yaml"}},
	}

	// Alias wins, resolved against the room file's directory: the room
	// sits in .loop/rooms/, its ../pipelines/ sibling is .loop/pipelines/.
	file, label, ok := s.resolveFile("implement")
	if !ok || label != "implement" || file != filepath.Join(dir, ".loop/rooms", "..", "pipelines", "implement.yaml") {
		t.Errorf("alias resolve = %q %q %v", file, label, ok)
	}

	// A path that exists: label from the base name.
	file, label, ok = s.resolveFile(".loop/pipelines/adhoc.yaml")
	if !ok || label != "adhoc" || file != ".loop/pipelines/adhoc.yaml" {
		t.Errorf("path resolve = %q %q %v", file, label, ok)
	}

	// Wrong suffix, missing file, unknown alias: not resolvable — and
	// the caller's error names all three remedies.
	for _, bad := range []string{".loop/pipelines/adhoc", ".loop/pipelines/missing.yaml", "not-an-alias"} {
		if _, _, ok := s.resolveFile(bad); ok {
			t.Errorf("%q should not resolve", bad)
		}
	}

	// Runnable: aliases first, then workspace pipeline files not behind
	// an alias (implement's file doesn't exist here, adhoc does).
	names := s.Runnable()
	if len(names) != 2 || names[0] != "implement" || names[1] != ".loop/pipelines/adhoc.yaml" {
		t.Errorf("Runnable() = %v, want [implement .loop/pipelines/adhoc.yaml]", names)
	}
}
