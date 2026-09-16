package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort reserves an ephemeral TCP port and releases it. There is a
// tiny TOCTOU window before serve binds it; acceptable in tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// shortHome is HOME for a serve under test: like t.TempDir but kept
// short — the derived socket path lives inside it, and unix socket
// binds cap paths at ~104 bytes, which go's test temp dir blows past.
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "loop-e2e-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startPortedServe launches `loop serve --port P --no-open` from dir
// with HOME=home (isolating the derived socket), output parked in a
// log file for failure reports.
func startPortedServe(t *testing.T, bin, dir, home string, port int) *exec.Cmd {
	t.Helper()
	logF, err := os.Create(filepath.Join(t.TempDir(), "serve.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--port", strconv.Itoa(port), "--no-open")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if data, err := os.ReadFile(logF.Name()); err == nil && len(data) > 0 {
			t.Logf("serve (port %d) output:\n%s", port, data)
		}
	})
	return cmd
}

// serveLog returns the captured output of a serve started by
// startPortedServe (best effort: the file may already be reaped).
func serveLog(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	if f, ok := cmd.Stdout.(*os.File); ok {
		if data, err := os.ReadFile(f.Name()); err == nil {
			return string(data)
		}
	}
	return ""
}

// waitPing polls the dashboard API until the daemon answers.
func waitPing(t *testing.T, port int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/ping", port)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			var m map[string]any
			err = json.NewDecoder(resp.Body).Decode(&m)
			_ = resp.Body.Close()
			if err == nil {
				return m
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon on port %d never answered /api/ping", port)
	return nil
}

// TestServePortTwoProjectsIsolated is the multi-project story end to
// end: two daemons, one per project directory, each dashboarded on its
// own port — no socket paths typed anywhere.
func TestServePortTwoProjectsIsolated(t *testing.T) {
	bin := loopBinary(t)
	portA, portB := freePort(t), freePort(t)
	for portB == portA {
		portB = freePort(t)
	}
	wsA, wsB := t.TempDir(), t.TempDir()
	homeA, homeB := shortHome(t), shortHome(t)
	startPortedServe(t, bin, wsA, homeA, portA)
	startPortedServe(t, bin, wsB, homeB, portB)

	pingA, pingB := waitPing(t, portA), waitPing(t, portB)
	if pingA["workspace"] != filepath.Base(wsA) {
		t.Errorf("daemon A workspace = %v, want %q", pingA["workspace"], filepath.Base(wsA))
	}
	if pingB["workspace"] != filepath.Base(wsB) {
		t.Errorf("daemon B workspace = %v, want %q", pingB["workspace"], filepath.Base(wsB))
	}

	// The sockets exist (the plumbing --port derives), each in its own
	// HOME so the two daemons never meet.
	for _, tc := range []struct {
		home string
		port int
	}{{homeA, portA}, {homeB, portB}} {
		sock := filepath.Join(tc.home, ".loop", fmt.Sprintf("daemon-%d.sock", tc.port))
		if _, err := os.Stat(sock); err != nil {
			t.Errorf("derived socket missing: %v", err)
		}
	}

	// The dashboard page itself carries the workspace brand.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", portA))
	if err != nil {
		t.Fatalf("fetching dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "loop · "+filepath.Base(wsA)) {
		t.Errorf("dashboard does not brand the workspace (want %q in page)", "loop · "+filepath.Base(wsA))
	}
}

// TestServePortTakenIsFriendly: a second dashboard on an occupied port
// fails with a human message, not a raw bind error.
func TestServePortTakenIsFriendly(t *testing.T) {
	bin := loopBinary(t)
	port := freePort(t)
	first := startPortedServe(t, bin, t.TempDir(), shortHome(t), port)
	waitPing(t, port)

	cmd := exec.Command(bin, "serve", "--port", strconv.Itoa(port), "--no-open")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+shortHome(t))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("second serve on port %d exited 0; first serve output:\n%s", port, serveLog(t, first))
	}
	if !strings.Contains(string(out), "already in use") {
		t.Fatalf("want a friendly port message, got:\n%s\nfirst serve output:\n%s", out, serveLog(t, first))
	}
}

// TestDashboardPickersOffersWorkspaceFiles: with conventional
// pipelines/ and rooms/ directories present, the dashboard offers
// pickers (labeled, prefilled) instead of a bare typed path.
func TestDashboardPickersOffersWorkspaceFiles(t *testing.T) {
	bin := loopBinary(t)
	port := freePort(t)
	ws, home := t.TempDir(), shortHome(t)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pipelines/release-check.yaml", "name: release-check\nstages: []\n")
	write("rooms/ops.yaml", "name: ops\nagents: []\n")

	startPortedServe(t, bin, ws, home, port)
	waitPing(t, port)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	page := string(body)
	for _, want := range []string{
		`<select class="picker" data-fills="run-file"`,
		`release-check · pipelines/release-check.yaml`,
		`<select class="picker" data-fills="host-file"`,
		`ops · rooms/ops.yaml`,
		`value="pipelines/release-check.yaml"`,
		`value="rooms/ops.yaml"`,
		`custom path…`,
		// lists are preloaded once and refreshed on demand, not polled
		`hx-get="/frag/workspace?kind=pipelines"`,
		`hx-get="/frag/workspace?kind=rooms"`,
		`hx-get="/frag/rooms" hx-trigger="load"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	// The picker refresh endpoint re-renders a single picker.
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/frag/workspace?kind=pipelines", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `data-fills="run-file"`) ||
		strings.Contains(string(body), `data-fills="host-file"`) {
		t.Errorf("frag/workspace?kind=pipelines = %d %q", resp.StatusCode, string(body))
	}
}

// TestServeSocketAndPortPickOne: the two daemon-naming flags are
// mutually exclusive (in-process: returns before touching any socket).
func TestServeSocketAndPortPickOne(t *testing.T) {
	cmd := newServeCmd()
	cmd.SetArgs([]string{"--port", "8787", "--socket", "/tmp/loop-test.sock"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("want mutual-exclusion error, got %v", err)
	}
}

// TestClientSocketNeedsDaemonMode: --socket on run/chat is daemon-only
// plumbing; without --daemon it teaches instead of being ignored.
func TestClientSocketNeedsDaemonMode(t *testing.T) {
	run := newRunCmd()
	run.SetArgs([]string{"--socket", "/tmp/loop-test.sock", "missing.yaml"})
	if err := run.Execute(); err == nil || !strings.Contains(err.Error(), "--socket only applies with --daemon") {
		t.Fatalf("run: want --socket guidance, got %v", err)
	}
	chat := newChatCmd()
	chat.SetArgs([]string{"--socket", "/tmp/loop-test.sock", "missing.yaml"})
	if err := chat.Execute(); err == nil || !strings.Contains(err.Error(), "--socket only applies with --daemon") {
		t.Fatalf("chat: want --socket guidance, got %v", err)
	}
}
