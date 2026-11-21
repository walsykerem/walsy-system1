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
