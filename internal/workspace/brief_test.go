package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBriefEmptyWorkspace(t *testing.T) {
	if b := Brief(t.TempDir()); b != "" {
		t.Errorf("empty workspace brief = %q, want empty", b)
	}
	if b := Brief(""); b != "" {
		t.Errorf("no dir brief = %q, want empty", b)
	}
}

func TestBriefStackDetection(t *testing.T) {
	cases := []struct {
		marker, content, want string
	}{
		{"go.mod", "module github.com/acme/webapp\n\ngo 1.27\n", "Go (module webapp)"},
		{"go.mod", "go 1.27\n", "Go"},
		{"package.json", `{"name": "acme-webapp", "version": "1.0.0"}`, "Node (acme-webapp)"},
		{"Cargo.toml", "[package]\nname = \"loader\"\n", "Rust (crate loader)"},
		{"pyproject.toml", "[project]\nname = \"etl\"\n", "Python (etl)"},
		{"requirements.txt", "flask==3\n", "Python"},
		{"Gemfile", "source 'https://rubygems.org'\n", "Ruby"},
	}
	for _, c := range cases {
		t.Run(c.marker, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.marker, c.content)
			b := Brief(dir)
			if !strings.Contains(b, c.want) {
				t.Errorf("brief missing %q:\n%s", c.want, b)
			}
			if !strings.Contains(b, "THIS project") {
				t.Errorf("brief missing its grounding header:\n%s", b)
			}
		})
	}
}

func TestBriefReadmeExcerpt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# loop\n\nA deterministic harness.\n\nMore detail follows.\n")
	b := Brief(dir)
	if !strings.Contains(b, "A deterministic harness.") {
		t.Errorf("brief missing readme excerpt:\n%s", b)
	}
	// The excerpt is the opening, not the whole file.
	if strings.Contains(b, "More detail follows") {
		t.Errorf("excerpt ran past the first paragraph:\n%s", b)
	}
	// Case-insensitive readme names count.
	dir2 := t.TempDir()
	write(t, dir2, "Readme.MD", "# x\n\nWords.\n")
	if b2 := Brief(dir2); !strings.Contains(b2, "Words.") {
		t.Errorf("Readme.MD not picked up:\n%s", b2)
	}
}

func TestBriefLayoutSkipsNoise(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n")
	for _, d := range []string{".git", "node_modules", ".loop", "vendor", "dist", "src", "rooms"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, ".env", "SECRET=1\n")
	b := Brief(dir)
	for _, noise := range []string{".git", "node_modules", ".loop", "vendor", "dist", ".env"} {
		if strings.Contains(b, noise) {
			t.Errorf("layout leaked noise %q:\n%s", noise, b)
		}
	}
	if !strings.Contains(b, "src/") || !strings.Contains(b, "rooms/") {
		t.Errorf("layout missing real entries:\n%s", b)
	}
}

func TestBriefCaps(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# big\n\n"+strings.Repeat("word ", 2000)+"\n")
	b := Brief(dir)
	if len(b) > maxBrief+16 {
		t.Errorf("brief len = %d, want ≤ %d", len(b), maxBrief)
	}
	if !strings.Contains(b, "…") {
		t.Errorf("a truncated excerpt should end with an ellipsis")
	}
}
