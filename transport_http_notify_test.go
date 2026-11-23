package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Streamable HTTP: a gated notification gets 202 with no body and no JSON
// content type; the same frame with an id keeps the JSON-RPC error reply.
func TestEnforcingGateNotificationHTTPNoReply(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	t.Setenv("WALSY_ROUTER_DENY", "read_file")

	h := mcpHTTPHandlerUpstream("")
	post := func(frame string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(frame))
		req.Header.Set(mcpProtocolVersionHeader, "2025-06-18")
		req.Header.Set(mcpMethodHeader, "tools/call")
		req.Header.Set(mcpNameHeader, "read_file")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := post(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read_file"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202; body %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("notification body = %q, want empty", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "application/json") {
		t.Fatalf("notification content-type = %q, want no JSON", ct)
	}
	if got := rec.Header().Get(mcpProtocolVersionHeader); got != "2025-06-18" {
		t.Fatalf("notification protocol header = %q", got)
	}
	if got := rec.Header().Get(mcpMethodHeader); got != "tools/call" {
		t.Fatalf("notification method mirror = %q", got)
	}
	if got := rec.Header().Get(mcpNameHeader); got != "read_file" {
		t.Fatalf("notification name mirror = %q", got)
	}

	rec = post(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read_file"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("request status = %d, want 200; body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("request content-type = %q, want JSON", ct)
	}
	var reply gateReply
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&reply); err != nil {
		t.Fatalf("request body not JSON: %v %q", err, rec.Body.String())
	}
	if reply.Error.Code != -32001 {
		t.Fatalf("request error code = %d, want -32001", reply.Error.Code)
	}
}
