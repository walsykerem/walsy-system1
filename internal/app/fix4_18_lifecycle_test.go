package app

// A14 lifecycle regression. Process-level and deterministic: the shutdown
// trigger is the client stream closing, never a real OS signal. The upstream
// is a shell that echoes one frame, then holds the same PID in `sleep` so it
// cannot exit on stdin EOF on its own. Without the fix the proxy hangs and the
// upstream PID survives; with it the frame is drained and the child is gone.

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fix418Env(t *testing.T, extra ...string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+len(extra)+4)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "HOME", "TMPDIR", "EVREN_API_KEY":
			continue
		}
		if strings.HasPrefix(name, "WALSY_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+t.TempDir(),
		"TMPDIR="+t.TempDir(),
		"WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT=http://127.0.0.1:1",
	)
	return append(env, extra...)
}

func TestFix418StdioShutdownDrainsAndKillsUpstream(t *testing.T) {
	bin := w04_build(t)
	pidFile := filepath.Join(t.TempDir(), "upstream.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// `cat` echoes the drained frame; `exec sleep` keeps the shell's PID alive
	// after cat sees EOF, so only explicit upstream cancellation can kill it.
	script := `echo $$ > "$FIX418_PIDFILE"; cat; exec sleep 60`
	cmd := exec.CommandContext(ctx, bin, "-transport", "stdio", "/bin/sh", "-c", script)
	cmd.Env = fix418Env(t, "FIX418_PIDFILE="+pidFile)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Drain proof: a frame written before disconnect must reach the upstream.
	frame := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	if _, err := io.WriteString(stdin, frame); err != nil {
		t.Fatal(err)
	}
	echo := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		echo <- line
	}()
	select {
	case line := <-echo:
		if !strings.Contains(line, `"method":"ping"`) {
			t.Fatalf("drained frame = %q, want the ping echo", line)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		t.Fatalf("upstream never echoed the frame; stderr=%s", stderr.String())
	}

	var pid int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, e := os.ReadFile(pidFile); e == nil {
			if n, e := strconv.Atoi(strings.TrimSpace(string(b))); e == nil && n > 0 {
				pid = n
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		_ = cmd.Process.Kill()
		t.Fatalf("upstream pid file missing; stderr=%s", stderr.String())
	}

	// Deterministic shutdown: client stream closes, no signal is sent.
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		t.Fatalf("proxy did not exit after client disconnect; stderr=%s", stderr.String())
	}

	// Child EXIT: the upstream PID must not survive its relay.
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("upstream pid %d survived client disconnect", pid)
	} else if err != syscall.ESRCH {
		t.Fatalf("probe upstream pid %d: %v", pid, err)
	}
}
