package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A JSON body served without nosniff can be sniffed as HTML by legacy browsers,
// turning attacker-controlled JSON-RPC strings into markup.
func TestJSONRPCHandlerSetsNosniff(t *testing.T) {
	routerReset()
	t.Cleanup(routerReset)
	t.Setenv("EVREN_API_KEY", "test-key")
	// Loopback stub: the handler's advisory decision must not reach the network.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	}))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig })

	h := mcpHTTPHandler()
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
