package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bfHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, routerDir), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func bfPost(h http.Handler, target, body, contentType string) *httptest.ResponseRecorder {
	req := dashRequest(http.MethodPost, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bfGet(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, dashRequest(http.MethodGet, target, nil))
	return rec
}

func bfOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"ok":true}` {
		t.Fatalf("body = %q, want {\"ok\":true}", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestLogsClearEndpointFlushesBothViews(t *testing.T) {
	home := bfHome(t)
	resetLogs()
	defer resetLogs()
	logCall("systemone", "ok", time.Millisecond, 10, 20, nil)
	secret := "sk-DO-NOT-PRINT-LOG"
	audit := filepath.Join(home, routerDir, routerAuditName)
	if err := os.WriteFile(audit, []byte(`{"event":"relay_frame","bucket":"`+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewDashboardHandler()

	rec := bfPost(h, "/api/logs/clear", `{}`, "application/json")
	bfOK(t, rec)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("clear response leaked audit text")
	}
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("audit file = %q, want empty", data)
	}
	total, entries := logsView()
	if total.Calls != 0 || len(entries) != 0 {
		t.Fatalf("in-memory logs survived: calls=%d entries=%d", total.Calls, len(entries))
	}
	page := bfGet(h, "/ui/logs").Body.String()
	if strings.Contains(page, secret) {
		t.Fatal("cleared audit row still rendered in /ui/logs")
	}
}

func TestLogsClearMissingAuditFileOK(t *testing.T) {
	home := bfHome(t)
	if err := os.Remove(filepath.Join(home, routerDir, routerAuditName)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	resetLogs()
	defer resetLogs()
	bfOK(t, bfPost(NewDashboardHandler(), "/api/logs/clear", `{}`, "application/json"))
	if _, err := os.Stat(filepath.Join(home, routerDir, routerAuditName)); !os.IsNotExist(err) {
		t.Fatal("clear created an audit file where none existed")
	}
}

func TestPromptsAddEndpointPersistsAndRenders(t *testing.T) {
	bfHome(t)
	h := NewDashboardHandler()
	bfOK(t, bfPost(h, "/api/prompts/add", `{"name":"E2E-PROMPT","text":"bind this prompt"}`, "application/json"))

	var ps []Prompt
	rec := bfGet(h, "/api/prompts")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ps) != nil {
		t.Fatalf("GET /api/prompts = %d: %s", rec.Code, rec.Body.String())
	}
	if len(ps) != 1 || ps[0].Name != "E2E-PROMPT" || ps[0].Text != "bind this prompt" {
		t.Fatalf("saved prompts = %+v", ps)
	}
	page := bfGet(h, "/ui/prompts").Body.String()
	if !strings.Contains(page, "E2E-PROMPT") {
		t.Fatal("new prompt missing from /ui/prompts")
	}
}

func TestPromptsAddEndpointRejectsBadInput(t *testing.T) {
	bfHome(t)
	h := NewDashboardHandler()
	cases := []struct {
		name, body, contentType string
	}{
		{"empty name", `{"name":"","text":"x"}`, "application/json"},
		{"empty text", `{"name":"X","text":" "}`, "application/json"},
		{"oversized text", fmt.Sprintf(`{"name":"X","text":"%s"}`, strings.Repeat("a", maxSystemPrompt+1)), "application/json"},
		{"oversized name", fmt.Sprintf(`{"name":"%s","text":"x"}`, strings.Repeat("n", maxPromptName+1)), "application/json"},
		{"form body", "name=X&text=y", "application/x-www-form-urlencoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := bfPost(h, "/api/prompts/add", tc.body, tc.contentType)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			if len(loadPrompts()) != 0 {
				t.Fatal("rejected prompt was persisted")
			}
		})
	}
}

func TestMutationRoutesGuardMethodOriginHostAndSecrets(t *testing.T) {
	bfHome(t)
	secret := "sk-DO-NOT-PRINT-KEY"
	keyPath, err := keyFilePath("SECRET-P")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewDashboardHandler()

	checks := []struct {
		name, target string
		mutate       func(*http.Request)
	}{
		{"logs GET", "/api/logs/clear", func(r *http.Request) { r.Method = http.MethodGet }},
		{"prompts GET", "/api/prompts/add", func(r *http.Request) { r.Method = http.MethodGet }},
		{"logs cross-origin", "/api/logs/clear", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{"prompts cross-origin", "/api/prompts/add", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{"logs off-host", "/api/logs/clear", func(r *http.Request) { r.Host = "evil.example" }},
		{"prompts off-host", "/api/prompts/add", func(r *http.Request) { r.Host = "evil.example" }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			req := dashRequest(http.MethodPost, tc.target, strings.NewReader(`{"name":"X","text":"y"}`))
			req.Header.Set("Content-Type", "application/json")
			tc.mutate(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatal("stored key leaked into error response")
			}
		})
	}
	for _, target := range []string{"/ui/logs", "/ui/prompts", "/api/logs"} {
		body := bfGet(h, target).Body.String()
		if strings.Contains(body, secret) {
			t.Fatalf("stored key leaked into %s", target)
		}
	}
}
