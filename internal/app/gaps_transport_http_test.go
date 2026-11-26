package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readFrame must accept a frame whose total length (newline included) is
// exactly the cap, and reject the first byte past it.
func TestGapTransportReadFrameSizeBoundary(t *testing.T) {
	atCap := append(bytes.Repeat([]byte("a"), maxStdioFrame-1), '\n')
	got, err := readFrame(bufio.NewReader(bytes.NewReader(atCap)))
	if err != nil || !bytes.Equal(got, atCap) {
		t.Fatalf("frame at cap: len=%d err=%v, want %d bytes and nil", len(got), err, maxStdioFrame)
	}

	overCap := append(bytes.Repeat([]byte("a"), maxStdioFrame), '\n')
	got, err = readFrame(bufio.NewReader(bytes.NewReader(overCap)))
	if err != errFrameTooLarge {
		t.Fatalf("frame past cap: err=%v, want errFrameTooLarge", err)
	}
	if got != nil {
		t.Fatalf("frame past cap returned %d bytes, want nil", len(got))
	}
}

// A final frame arriving without a newline must still be returned together
// with io.EOF; the unix no-upstream loop decides that last frame before exit.
func TestGapTransportReadFrameEOFWithTrailingFrame(t *testing.T) {
	last := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	got, err := readFrame(bufio.NewReader(bytes.NewReader(last)))
	if err != io.EOF || !bytes.Equal(got, last) {
		t.Fatalf("unterminated frame: got %q err=%v, want frame and io.EOF", got, err)
	}

	got, err = readFrame(bufio.NewReader(bytes.NewReader(nil)))
	if err != io.EOF || len(got) != 0 {
		t.Fatalf("empty input: got %q err=%v, want nil and io.EOF", got, err)
	}
}

// listenHTTP is loopback-only and validates the port before binding.
func TestGapTransportListenHTTPRejectsInvalidAddr(t *testing.T) {
	cases := []struct {
		addr string
		want error
	}{
		{"0.0.0.0:0", errNonLoopback},
		{"example.com:8080", errNonLoopback},
		{"127.0.0.1", nil}, // no port: SplitHostPort error
		{"127.0.0.1:abc", errInvalidPort},
		{"127.0.0.1:70000", errInvalidPort},
	}
	for _, tc := range cases {
		l, err := listenHTTP(tc.addr)
		if l != nil {
			l.Close()
		}
		if err == nil {
			t.Fatalf("listenHTTP(%q) succeeded, want error", tc.addr)
		}
		if tc.want != nil && err != tc.want {
			t.Fatalf("listenHTTP(%q) = %v, want %v", tc.addr, err, tc.want)
		}
	}
}

// Browser origins are gated by loopback, not by exact Host; absent Origin
// stays allowed for CLI clients. Both the predicate and the handler agree.
func TestGapTransportHTTPOriginPolicy(t *testing.T) {
	cases := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://127.0.0.1:9999", true},
		{"https://localhost:8443", true},
		{"http://[::1]:1234", true},
		{"http://evil.example", false},
		{"http://127.0.0.1.evil.example", false},
		{"ftp://127.0.0.1", false},
		{"://bad", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := originAllowed(r); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}

	post := func(origin string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		mcpHTTPHandler().ServeHTTP(rec, r)
		return rec.Code
	}
	if got := post("http://evil.example"); got != http.StatusForbidden {
		t.Fatalf("remote Origin status = %d, want 403", got)
	}
	// An allowed origin reaches JSON parsing: 400 (not 403) proves it passed
	// the origin gate without needing a live SO or upstream.
	if got := post("http://127.0.0.1:9999"); got != http.StatusBadRequest {
		t.Fatalf("loopback Origin status = %d, want 400 from body validation", got)
	}
}

// Handler error edges: malformed JSON-RPC, mismatched SEP-2243 headers and
// the Allow header on the rejected non-POST method.
func TestGapTransportHTTPHandlerErrorEdges(t *testing.T) {
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json")))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON-RPC") {
		t.Fatalf("malformed frame = %d %q, want 400 invalid JSON-RPC", rec.Code, rec.Body.String())
	}

	frame := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(frame))
	r.Header.Set(mcpMethodHeader, "initialize")
	rec = httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid MCP headers") {
		t.Fatalf("mismatched SEP header = %d %q, want 400 invalid MCP headers", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET = %d Allow=%q, want 405 Allow=POST", rec.Code, rec.Header().Get("Allow"))
	}
}

// macOS sun_path is 104 bytes; t.TempDir's path is too long for a socket.
func gapTransportSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gaptr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// A socket file left behind by a closed listener is stale and must be
// removable; the live-socket case is covered elsewhere.
func TestGapTransportUnixStaleSocketReplaced(t *testing.T) {
	path := gapTransportSocketPath(t, "stale.sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	ul, ok := l.(*net.UnixListener)
	if !ok {
		t.Fatalf("listenUnix returned %T, want *net.UnixListener", l)
	}
	ul.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file missing: %v", err)
	}

	l2, err := listenUnix(path)
	if err != nil {
		t.Fatalf("stale socket was not replaced: %v", err)
	}
	defer l2.Close()
	if l2.Addr().String() != path {
		t.Fatalf("replacement listener addr = %q, want %q", l2.Addr(), path)
	}
}

// A failed upstream dial on a unix connection is a transparent EOF: the
// server half-closes and drains so an in-flight client Write cannot break.
func TestGapTransportUnixDialFailureTransparentEOF(t *testing.T) {
	path := gapTransportSocketPath(t, "dial-fail.sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveUnix(l, func() (net.Conn, error) { return nil, errors.New("dial failed") })
	}()
	defer func() {
		l.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("serveUnix did not stop after listener close")
		}
	}()

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != io.EOF || n != 0 {
		t.Fatalf("dial failure reply: n=%d err=%v body=%q, want clean EOF and no frame", n, err, buf[:n])
	}
}

func gapTransportDashJSONRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:8787"
	r.Header.Set("Content-Type", "application/json")
	return r
}

// /api/keytest answers unconfigured providers as a request error, not as a
// failed key, and rejects a body without a provider before probing anything.
func TestGapTransportKeytestUnconfiguredAndInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gapTransportDashJSONRequest(http.MethodPost, "/api/keytest", `{"provider":"NO-SUCH","model":"m1"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("unconfigured provider status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	var out struct {
		Status int    `json:"status"`
		Result string `json:"result"`
		Winner string `json:"winner"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unconfigured provider body not JSON: %v %q", err, rec.Body.String())
	}
	if out.Status != http.StatusBadRequest || out.Result != "NO MODEL CONFIGURED" || out.Winner != "" {
		t.Fatalf("unconfigured provider = %+v, want 400/NO MODEL CONFIGURED/no winner", out)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, gapTransportDashJSONRequest(http.MethodPost, "/api/keytest", `{}`))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid request") {
		t.Fatalf("missing provider = %d %q, want 400 invalid request", rec.Code, rec.Body.String())
	}
}
