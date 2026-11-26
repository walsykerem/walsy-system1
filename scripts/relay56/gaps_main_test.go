package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gapRelayWriter is the minimal io.WriteCloser handshake needs; it keeps the
// written frame instead of a live relay.
type gapRelayWriter struct{ bytes.Buffer }

func (w *gapRelayWriter) Close() error { return nil }

// handshake must emit one initialize frame with the given id.
func TestGapRelayHandshakeWritesInitialize(t *testing.T) {
	old := replyTimeout
	replyTimeout = 20 * time.Millisecond
	defer func() { replyTimeout = old }()

	w := &gapRelayWriter{}
	c := &client{in: w, waiters: make(map[int]chan map[string]any)}
	handshake(c, 4)

	got := w.String()
	if !strings.Contains(got, `"id":4`) || !strings.Contains(got, `"method":"initialize"`) {
		t.Fatalf("handshake frame = %q, want an initialize frame with id 4", got)
	}
}

// gapRelaySubprocess re-executes this test binary with one env flag set so a
// function that calls os.Exit runs without killing the test process.
func gapRelaySubprocess(t *testing.T, testName, envKey string, timeout time.Duration) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), envKey+"=1", "RELAY56_BIN=")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s timed out after %s\n%s", testName, timeout, out)
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %s: %v\n%s", testName, err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// die prints the FAIL line and exits 2; both are asserted from a child process.
func TestGapRelayDieExitsTwo(t *testing.T) {
	out, code := gapRelaySubprocess(t, "TestGapRelayDieHelper", "RELAY56_GAP_DIE_HELPER", 30*time.Second)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL harness:") {
		t.Fatalf("output lacks the FAIL harness line:\n%s", out)
	}
}

func TestGapRelayDieHelper(t *testing.T) {
	if os.Getenv("RELAY56_GAP_DIE_HELPER") != "1" {
		return
	}
	die(errors.New("gap relay die probe"))
}

// buildTestBin must produce a runnable binary from the current source.
func TestGapRelayBuildTestBinProducesExecutable(t *testing.T) {
	out := buildTestBin("http://127.0.0.1:1/v1/systemone")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(out)) })

	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("built binary %s: %v", out, err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("built binary mode = %v, want an executable regular file", st.Mode())
	}
	if st.Size() == 0 {
		t.Fatalf("built binary %s is empty", out)
	}
}

// spawn must start the relay with an isolated HOME and stop cleanly.
func TestGapRelaySpawnStartsChild(t *testing.T) {
	built := buildTestBin("http://127.0.0.1:1/v1/systemone")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(built)) })

	orig := bin
	bin = built
	defer func() { bin = orig }()

	old := replyTimeout
	replyTimeout = 3 * time.Second
	defer func() { replyTimeout = old }()

	c := spawn("http://127.0.0.1:1/v1/systemone")
	if c == nil {
		t.Fatal("spawn returned nil")
	}
	if c.home == "" {
		t.Fatal("spawn did not isolate HOME")
	}
	handshake(c, 1)
	c.close()

	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not stop after close")
	}
	if _, err := os.Stat(c.home); !os.IsNotExist(err) {
		t.Fatalf("private HOME %s not removed: %v", c.home, err)
	}
}

// main runs the whole transcript; its exit status and RELAY-OK come from a
// child process so a failed check cannot exit this test binary.
func TestGapRelayMainTranscript(t *testing.T) {
	out, code := gapRelaySubprocess(t, "TestGapRelayMainHelper", "RELAY56_GAP_MAIN_HELPER", 8*time.Minute)
	if code != 0 {
		t.Fatalf("main exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "RELAY-OK") {
		t.Fatalf("main output lacks RELAY-OK:\n%s", out)
	}
}

func TestGapRelayMainHelper(t *testing.T) {
	if os.Getenv("RELAY56_GAP_MAIN_HELPER") != "1" {
		return
	}
	main()
	// The coverage runtime flushes its counters at os.Exit, but not when the
	// testing framework finishes normally, so exit explicitly to keep main and
	// everything it calls in the parent's profile.
	os.Exit(0)
}
