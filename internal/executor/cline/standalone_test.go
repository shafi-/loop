package cline

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/executor"
)

// makeExec writes an executable placeholder (a sh script) at path.
func makeExec(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The standalone host satisfies Check by itself and Run must exec it
// directly — even with a NodeBin that cannot exist.
func TestStandaloneHostSatisfiesCheckAndRun(t *testing.T) {
	dir := t.TempDir()
	bin := makeExec(t, filepath.Join(dir, "host"), `cat > /dev/null; echo '{"type":"done","output":"standalone"}'; exit 0`)
	e := &Executor{NodeBin: "/nonexistent/node", HostPath: filepath.Join(dir, "index.mjs"), HostBin: bin}

	if !e.StandaloneHost() {
		t.Fatal("StandaloneHost must detect the executable")
	}
	if problems := e.Check(); len(problems) != 0 {
		t.Fatalf("Check with a standalone host = %v, want none", problems)
	}
	res, err := e.Run(t.Context(), taskFixture(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "standalone" {
		t.Errorf("output = %q", res.Output)
	}
}

// A non-executable or directory at HostBin is not a standalone host.
func TestStandaloneHostRejectsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "host")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{HostBin: notExec}
	if e.StandaloneHost() {
		t.Error("a 0644 file must not count as a standalone host")
	}
	if problems := e.Check(); len(problems) == 0 {
		t.Error("without a standalone host or node, Check must report problems")
	}
}

func TestBunAssetMatrix(t *testing.T) {
	for _, c := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "bun-darwin-aarch64.zip"},
		{"darwin", "amd64", "bun-darwin-x64.zip"},
		{"linux", "arm64", "bun-linux-aarch64.zip"},
		{"linux", "amd64", "bun-linux-x64.zip"},
	} {
		got, err := bunAsset(c.goos, c.goarch)
		if err != nil || got != c.want {
			t.Errorf("bunAsset(%s/%s) = %q, %v; want %s", c.goos, c.goarch, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{"windows", "amd64"}, {"darwin", "386"}} {
		if _, err := bunAsset(c[0], c[1]); err == nil {
			t.Errorf("bunAsset(%s/%s) must be unsupported", c[0], c[1])
		}
	}
}

func TestExtractBunFindsNestedBinary(t *testing.T) {
	dir := t.TempDir()
	zpath := filepath.Join(dir, "bun-test.zip")
	// Bun's zips have varied layouts; write one with the binary one
	// directory deep to pin the "any regular file named bun" rule.
	zf, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(zf)
	f, err := w.Create("bun-darwin-aarch64/bun")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("#!/bin/sh\necho fake-bun\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()

	dest := t.TempDir()
	if err := extractBun(zpath, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "bun"))
	if err != nil || !strings.Contains(string(got), "fake-bun") {
		t.Errorf("extracted bun = %q (%v)", got, err)
	}
	fi, err := os.Stat(filepath.Join(dest, "bun"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("extracted bun must be executable: %v", err)
	}
}

func TestSmokeHost(t *testing.T) {
	// A well-behaved host: answers the protocol error shape.
	good := makeExec(t, filepath.Join(t.TempDir(), "host"), `
echo '{"type":"error","message":"malformed task line: oops"}'
exit 1
`)
	if err := smokeHost(good); err != nil {
		t.Errorf("good host smoke failed: %v", err)
	}
	// A broken host: wrong shape.
	bad := makeExec(t, filepath.Join(t.TempDir(), "host"), `echo "segmentation fault (core dumped)"; exit 139`)
	if err := smokeHost(bad); err == nil {
		t.Error("a broken host must fail its smoke test")
	}
}

// taskFixture is the minimal valid Task for Run tests.
func taskFixture(t *testing.T) executor.Task {
	t.Helper()
	return executor.Task{Instruction: "smoke", Model: executor.ModelSpec{Provider: "anthropic", Model: "test"}}
}
