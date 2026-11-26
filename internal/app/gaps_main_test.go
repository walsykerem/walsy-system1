package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gapMainCapture swaps os.Stdout/os.Stderr for pipes around fn and returns
// everything written to each.
func gapMainCapture(t *testing.T, fn func()) (string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		_ = outR.Close()
		_ = errR.Close()
	}()

	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	fn()

	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	return <-outCh, <-errCh
}

// -print-client-config must print a usable snippet and return, not exit.
func TestGapMainPrintClientConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	*flagPrintConfig = false
	t.Cleanup(func() { *flagPrintConfig = false })
	os.Args = []string{"walsy-system1", "-print-client-config"}

	stdout, _ := gapMainCapture(t, Main)
	if !*flagPrintConfig {
		t.Fatal("main did not parse -print-client-config")
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("print-client-config wrote no config")
	}
	var cfg struct {
		MCP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("printed config is not JSON: %v; output %q", err, stdout)
	}
	if _, ok := cfg.MCP.Servers["walsy-system1"]; !ok {
		t.Fatalf("printed config lacks walsy-system1 server: %q", stdout)
	}
}

// prefixWriter tags upstream stderr so it cannot be mistaken for a metric line.
func TestGapMainPrefixWriter(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: []byte("upstream: ")}
	payload := []byte("boom\n")
	n, err := p.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(payload))
	}
	if got, want := buf.String(), "upstream: boom\n"; got != want {
		t.Fatalf("Write output = %q, want %q", got, want)
	}
}

// runStdio must relay one frame through a real short-lived upstream (cat).
func TestGapMainRunStdioRelaysThroughCat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	oldMetric := metricOut
	metricOut = io.Discard
	t.Cleanup(func() { metricOut = oldMetric })

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		_ = inR.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runStdio([]string{"/bin/cat"})
	}()

	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	if _, err := inW.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(outR).ReadString('\n')
		lineCh <- line
	}()
	select {
	case got := <-lineCh:
		if got != frame {
			t.Fatalf("relayed frame = %q, want %q", got, frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the relayed frame")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runStdio did not return after upstream EOF")
	}
}

// macOS sun_path is 104 bytes, so keep the socket directory short.
func gapMainSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gapmain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// runUnix blocks by design: prove the socket serves once, then leave it.
func TestGapMainRunUnixServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := gapMainSocketPath(t, "s.sock")
	t.Cleanup(func() { _ = os.Remove(path) })

	go runUnix(path, "")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unix socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix socket: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close unix conn: %v", err)
	}
}

// runHTTP blocks by design: poll a loopback echo request until it answers.
func TestGapMainRunHTTPEchoes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}

	go runHTTP(addr, "")

	client := &http.Client{Timeout: time.Second}
	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Post("http://"+addr+"/", "application/json", strings.NewReader(frame))
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			last = readErr
		} else if resp.StatusCode != http.StatusOK || string(body) != frame {
			last = fmt.Errorf("status %d body %q", resp.StatusCode, body)
		} else {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("runHTTP never echoed on %s: %v", addr, last)
}
