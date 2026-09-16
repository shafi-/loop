package cline

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// Install writes the embedded host files to the install target and makes
// them runnable, best form first:
//
//  1. dependencies (@cline/sdk) — via bun (found or fetched), else npm
//     when a usable Node exists;
//  2. the standalone host — `bun build --compile`, then a smoke test;
//     if either fails, the script mode remains (index.mjs + SDK) and a
//     usable Node (≥ 22) or bun runs it.
//
// Network is required on first install. Idempotent: re-running refreshes
// the host files and upgrades the SDK.
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

	// Dependencies. Bun is preferred: it ignores the package's node
	// engines field and is the same toolchain that compiles below.
	// Lockfiles are removed first: loop ships none, so any lockfile in
	// the target is stale — and bun refuses to reconcile a changed
	// lockfile when it decides to freeze.
	bun := FindBun()
	if bun == "" {
		fmt.Fprintln(os.Stderr, "· fetching the bun toolchain (one-time, ~30 MB)…")
		bun, err = InstallBun()
		if err != nil {
			bun = ""
			fmt.Fprintf(os.Stderr, "! bun download failed (%v) — falling back to npm\n", err)
		}
	}
	for _, lock := range []string{"bun.lock", "bun.lockb", "package-lock.json"} {
		_ = os.Remove(filepath.Join(dir, lock))
	}
	if bun != "" {
		if err := runIn(dir, bun, "install", "--production", "--no-progress"); err != nil {
			return dir, fmt.Errorf("bun install failed in %s: %w", dir, err)
		}
	} else {
		npm, err := exec.LookPath("npm")
		if err != nil {
			return dir, fmt.Errorf("host files written to %s but neither bun nor npm is available: install Node.js or run `loop setup`", dir)
		}
		if err := runIn(dir, npm, "install", "--omit=dev", "--no-audit", "--no-fund"); err != nil {
			return dir, fmt.Errorf("npm install failed in %s: %w", dir, err)
		}
	}

	// Standalone host: compile and smoke it. Failure is not fatal —
	// the script mode above already works.
	if bun != "" {
		if err := compileHost(dir, bun); err != nil {
			fmt.Fprintf(os.Stderr, "! standalone host unavailable (%v) — the script mode will be used\n", err)
		} else if err := smokeHost(filepath.Join(dir, "host")); err != nil {
			fmt.Fprintf(os.Stderr, "! standalone host failed its smoke test (%v) — removed; the script mode will be used\n", err)
			_ = os.Remove(filepath.Join(dir, "host"))
		}
	}
	return dir, nil
}

// compileHost produces the standalone binary: bun bundles index.mjs and
// the SDK into one self-contained executable.
func compileHost(dir, bun string) error {
	host := filepath.Join(dir, "host")
	if err := runIn(dir, bun, "build", "--compile", "index.mjs", "--outfile", host); err != nil {
		return err
	}
	if !usableExecutable(host) {
		return fmt.Errorf("compile produced no executable at %s", host)
	}
	return nil
}

// smokeHost proves a compiled host actually executes its bundled module
// graph: fed a malformed task line it must answer with the protocol's
// error shape. The exit code is 1 by design on that path (the host
// exits non-zero on protocol errors), so only the output shape — and
// finishing within the timeout — are asserted. No network, no LLM.
func smokeHost(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stdin = strings.NewReader("this is not json\n")
	out, _ := cmd.CombinedOutput() // exit code ignored: malformed input exits 1 by design
	if ctx.Err() != nil {
		return fmt.Errorf("host did not finish within the smoke timeout")
	}
	if !strings.Contains(string(out), `"type":"error"`) || !strings.Contains(string(out), "malformed task line") {
		return fmt.Errorf("unexpected smoke output: %s", firstLine(string(out)))
	}
	return nil
}

// runIn executes a command in dir, streaming output.
func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}
