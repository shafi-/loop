// Bun toolchain fetching: `loop setup` downloads a bun release binary
// so it can install the cline SDK and compile the standalone host
// without requiring Node on the user's machine. Bun is a build/install
// tool here, not a runtime: once the host is compiled, nothing needs it.
package cline

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// bunAsset maps the running platform to bun's release asset name
// (https://github.com/oven-sh/bun/releases). loop supports darwin/linux
// on amd64/arm64 — the same set install.sh does.
func bunAsset(goos, goarch string) (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[goarch]
	if arch == "" {
		return "", fmt.Errorf("bun has no build for this architecture (%s/%s)", goos, goarch)
	}
	switch goos {
	case "darwin", "linux":
		return fmt.Sprintf("bun-%s-%s.zip", goos, arch), nil
	}
	return "", fmt.Errorf("bun has no build for %s", goos)
}

// bunInstallTarget is where setup places the fetched binary: inside
// loop's own state dir, self-contained and removable with it.
func bunInstallTarget() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loop", "bin", "bun"), nil
}

// minBunMajor is the toolchain floor: `bun build --compile` (the
// standalone host) landed in bun 1.1, and 1.0.x installs behave
// differently (unconditional lockfile freezing). A PATH bun older than
// this is ignored; setup fetches a current one instead.
const minBunMajor = 1
const minBunMinor = 1

// FindBun locates a usable bun: LOOP_BUN env, PATH, then loop's own
// install target — each accepted only if it reports version ≥ 1.1.
// Empty when none qualifies.
func FindBun() string {
	candidates := []string{}
	if p := os.Getenv("LOOP_BUN"); p != "" {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("bun"); err == nil {
		candidates = append(candidates, p)
	}
	if p, err := bunInstallTarget(); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if usableExecutable(p) && bunVersionAtLeast(p, minBunMajor, minBunMinor) {
			return p
		}
	}
	return ""
}

// bunVersionAtLeast asks the binary for its version and compares
// semver-ish "1.2.3" against the floor.
func bunVersionAtLeast(bin string, major, minor int) bool {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return false
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), ".", 3)
	if len(fields) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(fields[0])
	min, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && min >= minor)
}

// InstallBun downloads the bun release binary for this platform and
// installs it at ~/.loop/bin/bun. Returns the path. Idempotent.
func InstallBun() (string, error) {
	target, err := bunInstallTarget()
	if err != nil {
		return "", err
	}
	if usableExecutable(target) {
		return target, nil
	}
	asset, err := bunAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	url := "https://github.com/oven-sh/bun/releases/latest/download/" + asset
	tmp, err := os.MkdirTemp("", "loop-bun-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	zpath := filepath.Join(tmp, asset)
	if err := download(url, zpath); err != nil {
		return "", fmt.Errorf("downloading bun: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := extractBun(zpath, filepath.Dir(target)); err != nil {
		return "", fmt.Errorf("extracting bun: %w", err)
	}
	if !usableExecutable(target) {
		return "", fmt.Errorf("bun archive did not contain a usable binary at %s", target)
	}
	return target, nil
}

// extractBun pulls the bun executable out of the release zip. The zip
// layout has varied across releases (root or one directory deep), so
// any regular file named "bun" is accepted.
func extractBun(zpath, destDir string) error {
	r, err := zip.OpenReader(zpath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) != "bun" || f.FileInfo().IsDir() {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.OpenFile(filepath.Join(destDir, "bun"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	}
	return fmt.Errorf("no bun executable in %s", filepath.Base(zpath))
}
