// The starter kit's filesystem side: seeding the shipped personas into
// the global library so every project can reference them. Content
// lives in internal/examples (embedded); this file only puts it on
// disk, idempotently and without ever clobbering a user's edits.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/examples"
)

// seedGlobalPersonas writes the shipped personas (architect, engineer,
// reviewer, product-owner, cfo, end-user) into ~/.loop/personas/ — the
// library every project reads. Idempotent: an existing file is never
// overwritten, so a persona the user edited stays edited (delete the
// file to restore the built-in).
func seedGlobalPersonas(out, warn io.Writer) {
	dir, err := config.GlobalPersonaDir()
	if err != nil {
		fmt.Fprintf(warn, "· persona library unavailable: %v\n", err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(warn, "· persona library unavailable: %v\n", err)
		return
	}
	names := make([]string, 0, len(examples.BuiltinPersonas()))
	for name := range examples.BuiltinPersonas() {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name+".yaml")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(examples.BuiltinPersonas()[name]), 0o644); err != nil {
			fmt.Fprintf(warn, "· could not write %s: %v\n", path, err)
			continue
		}
		fmt.Fprintf(out, "✓ persona %s → %s\n", name, path)
	}
}
