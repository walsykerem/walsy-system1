package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Acceptance checks for the seven contract gaps. All state is local: temp HOME,
// no live System One call, no key material in assertions or output.

func TestAcceptanceGap5HTTPOriginAndMethod(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"method", func(r *http.Request) { r.Header.Set("Mcp-Method", "tools/call") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
			tc.mut(req)
			mcpHTTPHandler().ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("bad %s accepted with 200", tc.name)
			}
		})
	}
}
