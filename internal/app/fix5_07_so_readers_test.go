package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFix5_07_SOStateReadersOriginGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	paths := []string{
		"/", "/api/health", "/api/feedback", "/api/providers", "/api/logs",
		"/api/prompts", "/api/system-prompt", "/api/combo",
		"/ui/logs", "/ui/prompts", "/ui/combo/sheet", "/ui/combo/add/sheet",
	}
	for _, p := range paths {
		for _, tc := range []struct {
			name, origin string
			want         int
		}{
			{"cross-origin", "http://evil.example", http.StatusBadRequest},
			{"same-origin", "http://127.0.0.1:8787", http.StatusOK},
			{"no-origin", "", http.StatusOK},
		} {
			t.Run(p+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, p, nil)
				req.Host = "127.0.0.1:8787"
				if tc.origin != "" {
					req.Header.Set("Origin", tc.origin)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("GET %s Origin=%q = %d, want %d: %s", p, tc.origin, rec.Code, tc.want, rec.Body.String())
				}
			})
		}
	}
	for p, want := range map[string]int{"/style.css": http.StatusOK, "/favicon.ico": http.StatusNoContent} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Host = "127.0.0.1:8787"
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("GET %s cross-origin = %d, want %d: %s", p, rec.Code, want, rec.Body.String())
		}
	}
}
