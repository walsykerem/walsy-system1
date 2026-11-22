package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// HTTP POST JSON-RPC: body echoed verbatim, MCP-Protocol-Version round-trips,
// no SSE, 127.0.0.1:0 yields a real ephemeral port > 0.
func TestHTTPPostJSONRPC(t *testing.T) {
	l, err := listenHTTP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	addr := l.Addr().String()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if !isLoopbackHost(host) {
		t.Fatalf("listen host %q not loopback", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		t.Fatalf("ephemeral port = %q", portStr)
	}

	srv := &http.Server{Handler: mcpHTTPHandler()}
	go srv.Serve(l)
	defer srv.Close()

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr, bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(mcpProtocolVersionHeader, "2025-06-18")
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(mcpProtocolVersionHeader); got != "2025-06-18" {
		t.Fatalf("protocol header = %q", got)
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "event-stream") {
		t.Fatalf("SSE leaked: %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, frame) {
		t.Fatalf("body = %q, want verbatim %q", body, frame)
	}
	var rpc map[string]any
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("not JSON-RPC: %v", err)
	}
}

// GET is rejected: Streamable HTTP only, no SSE stream endpoint.
func TestHTTPGetNoSSE(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}
}

// Upstream mode proxies the frame; no upstream preserves legacy echo.
func TestHTTPUpstreamProxy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	defer routerReset()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"tools/call"`)) {
			t.Errorf("upstream got %q", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"upstream":true}}`))
	}))
	defer up.Close()

	frame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x"}}`)
	rec := httptest.NewRecorder()
	mcpHTTPHandlerUpstream(up.URL).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"upstream":true`)) {
		t.Fatalf("upstream body = %q", rec.Body.String())
	}
}

// Browser origin mismatch and SEP-2243 method mismatch are rejected, never 200.
func TestHTTPHeaderGuards(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"bad-origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{"bad-method", func(r *http.Request) { r.Header.Set(mcpMethodHeader, "initialize") }},
		{"bad-name", func(r *http.Request) { r.Header.Set(mcpNameHeader, "other") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
			tc.mut(r)
			mcpHTTPHandler().ServeHTTP(rec, r)
			if rec.Code == http.StatusOK {
				t.Fatalf("bad %s accepted with 200", tc.name)
			}
		})
	}
}

// Matching SEP-2243 headers are mirrored on the response.
func TestHTTPSEP2243Mirror(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	defer routerReset()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
	r.Header.Set(mcpMethodHeader, "tools/call")
	r.Header.Set(mcpNameHeader, "x")
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get(mcpMethodHeader) != "tools/call" || rec.Header().Get(mcpNameHeader) != "x" {
		t.Fatalf("SEP headers not mirrored: %v", rec.Header())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A body that cannot be read must answer 400, not fall through to an unwritten
// status (which net/http renders as a 200 with no frame).
func TestHTTPUnreadableBodyRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = io.NopCloser(failingReader{})
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body: status = %d, want 400", rec.Code)
	}
}
