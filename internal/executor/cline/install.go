package cline

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"os/exec"
)

//go:embed assets/index.mjs assets/package.json
var assets embed.FS

// InstallTarget is where `loop executor install cline` places the host.
func InstallTarget() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".loop", "executors", "cline"), nil
}

// Install writes the embedded host files to the install target and runs
// `npm install` there. Network is required for the npm step.
func Install() (string, error) {
	dir, err := InstallTarget()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"index.mjs", "package.json"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return "", err
		}
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return dir, fmt.Errorf("host files written to %s but npm not found: run npm install there manually", dir)
	}
	cmd := exec.Command(npm, "install", "--omit=dev", "--no-audit", "--no-fund")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return dir, fmt.Errorf("npm install failed in %s: %w", dir, err)
	}
	return dir, nil
}
