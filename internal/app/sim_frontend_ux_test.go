package app

// Focused frontend E2E contracts for the loopback dashboard. Static template
// and stylesheet assertions are deliberate: the endpoints are implemented and
// verified by the backend agent, and these tests must not mutate live state.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func feuxFile(t *testing.T, name string) string {
	t.Helper()
	dir := findSourceDir()
	if dir == "" {
		dir = "."
	}
	b, err := os.ReadFile(filepath.Join(dir, "internal", "app", "web", name))
	if err != nil {
		t.Fatalf("read internal/app/web/%s: %v", name, err)
	}
	return string(b)
}

func feuxRender(t *testing.T, target string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", target, rec.Code)
	}
	return rec.Body.String()
}

func feuxHas(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("missing %q", needle)
	}
}

// No feedback UI: the read-only feedback surface was removed in FA14.5 and
// must never reappear in the dashboard or either dynamic sheet.
func feuxNoFeedback(t *testing.T, page string) {
	t.Helper()
	for _, marker := range []string{`id="feedback"`, "feedback (read-only)", "feedback-text", ">FEEDBACK<"} {
		if strings.Contains(strings.ToLower(page), strings.ToLower(marker)) {
			t.Errorf("feedback UI marker %q returned", marker)
		}
	}
}

// The dashboard header must not contain the loopback origin URL; the brand
// stays as the home link and sheets keep no topbar duplication.
func TestFEUXNoOriginLinkInTopbar(t *testing.T) {
	page := feuxRender(t, "/")
	feuxNoFeedback(t, page)
	feuxHas(t, page, `<header class="topbar">`)
	for _, gone := range []string{`class="origin-link"`, `http://127.0.0.1:8787/`, `Dashboard origin`} {
		if strings.Contains(page, gone) {
			t.Errorf("home still renders %q", gone)
		}
	}
	css := feuxFile(t, "style.css")
	if strings.Contains(css, ".origin-link") {
		t.Error("style.css still defines .origin-link")
	}
	// Sheets themselves must stay sheet-only (no home topbar duplication).
	sheet := feuxRender(t, "/ui/prompts")
	if strings.Contains(sheet, `class="topbar"`) {
		t.Error("prompts sheet rendered the home topbar")
	}
}

// LOGS clear: confirm first, POST no body, short success/error toast, and a
// local row replacement so the sheet never reloads on a successful clear.
func TestFEUXLogsClearContract(t *testing.T) {
	html := feuxRender(t, "/ui/logs")
	feuxNoFeedback(t, html)
	for _, needle := range []string{
		`data-action="logs-clear"`, `>CLEAR LOGS</button>`,
		`window.confirm('Clear all logs? This cannot be undone.')`,
		`fetch('/api/logs/clear', { method: 'POST', cache: 'no-store' })`,
		`if (!j.ok) throw new Error('not ok');`,
		`NO CALLS YET.`,
		`showToast('LOGS CLEARED', true)`,
		`showToast('LOGS CLEAR FAILED', false)`,
	} {
		feuxHas(t, html, needle)
	}
}

// Cold deep links must resolve from location.hash, not :target: Chrome has not
// resolved :target while the inline script parses, so GET /#logs used to leave
// the iframe at about:blank. Danger buttons need their own token and selector
// to survive the later .btn background rule.
func TestFEUXColdHashLoadAndDangerButton(t *testing.T) {
	html := feuxFile(t, "index.html")
	for _, needle := range []string{
		`var t = location.hash && document.getElementById(decodeURIComponent(location.hash.slice(1)));`,
		`if (t) loadSheetFrame(t);`,
	} {
		feuxHas(t, html, needle)
	}
	if strings.Contains(html, `if (document.querySelector(':target')) loadSheetFrame`) {
		t.Error("cold load still relies on parse-time :target")
	}

	css := feuxFile(t, "style.css")
	_, tail, ok := strings.Cut(css, ":root {")
	if !ok {
		t.Fatal(":root block missing")
	}
	root, _, _ := strings.Cut(tail, "}")
	if !strings.Contains(root, "--danger:") {
		t.Errorf("--danger not defined in :root: %q", root)
	}
	if !strings.Contains(css, ".btn.btn-danger {") || strings.Contains(css, "\n.btn-danger {") {
		t.Error(".btn-danger must be scoped as .btn.btn-danger so .btn cannot override it")
	}
	feuxHas(t, feuxRender(t, "/ui/logs"), `class="btn btn-small btn-danger"`)
}

// On a 375px sheet keep the panel below the viewport top and within bounds.
func TestFEUXMobileSheetOffset(t *testing.T) {
	css := feuxFile(t, "style.css")
	i := strings.Index(css, "@media (max-width: 720px)")
	if i < 0 {
		t.Fatal("mobile media query missing")
	}
	query := css[i:]
	if j := strings.Index(query, "\n}"); j >= 0 {
		query = query[:j]
	}
	for _, want := range []string{
		`body:has(.sheet-backdrop:target) .sheet`,
		`margin-top: 44px`,
		`max-height: calc(100dvh - 44px)`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("mobile query missing %q", want)
		}
	}
}

// Prompts add: the existing form is enhanced with the exact JSON contract
// (name + text), no-store, and an in-place row insert. Plain POST remains the
// no-JS fallback.
func TestFEUXPromptsAddContract(t *testing.T) {
	html := feuxRender(t, "/ui/prompts")
	feuxNoFeedback(t, html)
	for _, needle := range []string{
		`data-prompt-add`,
		`action="/ui/prompts/add"`,
		`fetch('/api/prompts/add'`,
		`'Content-Type': 'application/json'`,
		`JSON.stringify({ name: name, text: text })`,
		`cache: 'no-store'`,
		`if (!j.ok) throw new Error('not ok');`,
		`showToast('PROMPT ADDED', true)`,
		`showToast('PROMPT ADD FAILED', false)`,
	} {
		feuxHas(t, html, needle)
	}
	if strings.Contains(html, `JSON.stringify({text:`) {
		t.Error("prompts add still uses the text-only body the API rejects")
	}
}
