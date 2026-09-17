package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// Room agents are grounded in the workspace they were hosted from: the
// brief is assembled from the daemon's working directory and set on
// every agent, so CLI-hosted and daemon-hosted rooms see the same
// project context.
func TestBuildRoomAgentsCarriesWorkspaceBrief(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1") // never called
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/acme/webapp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# webapp\n\nThe billing service.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	room := &config.Room{Name: "demo", Agents: []config.Persona{{Name: "scout", System: "You scout."}}}
	agents, err := buildRoomAgents(room)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d", len(agents))
	}
	brief := agents[0].Workspace
	for _, want := range []string{"THIS project", "Go (module webapp)", "The billing service."} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q:\n%s", want, brief)
		}
	}
}
