package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCoreNeverImportsCLI pins the layering rule of the house: internal/cli
// is the composition root — the only place that wires surfaces (run, chat,
// ui) to the core — so no core package may import it. If this test fails,
// a surface concern leaked into the core and the dependency arrow points
// the wrong way.
//
// TestWebUIIsPureClient (same walk) additionally pins webui to its API
// boundary: it may talk to the daemon client only, never to the engine
// or the supervisor directly.
func TestCoreNeverImportsCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to go list")
	}
	goBin := os.Getenv("GO_BIN")
	if goBin == "" {
		goBin = "go"
	}
	cmd := exec.Command(goBin, "list", "-f", `{{.ImportPath}}|{{join .Imports " "}}|{{join .TestImports " "}}`, "./internal/...")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	const cliPkg = "github.com/shafi-/loop/internal/cli"
	webuiPkg := "github.com/shafi-/loop/internal/webui"
	forbiddenInWebUI := map[string]bool{
		"github.com/shafi-/loop/internal/engine": true,
		"github.com/shafi-/loop/internal/runctl": true,
		"github.com/shafi-/loop/internal/cli":    true,
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		pkg := parts[0]
		for _, imports := range []string{parts[1], parts[2]} {
			for _, imp := range strings.Fields(imports) {
				if pkg != cliPkg && imp == cliPkg {
					t.Errorf("%s imports internal/cli — the core must never import the CLI", pkg)
				}
				if pkg == webuiPkg && forbiddenInWebUI[imp] {
					t.Errorf("webui imports %s — the UI is a pure daemon-API client", imp)
				}
			}
		}
	}
}
