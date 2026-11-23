package main

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
	b, err := os.ReadFile(filepath.Join(dir, "web", name))
	if err != nil {
		t.Fatalf("read web/%s: %v", name, err)
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

// The origin address must live in the home topbar and float above an open
// sheet's backdrop, so #prompts / #logs never hide the way back home.
func TestFEUXOriginLinkAlwaysVisible(t *testing.T) {
	page := feuxRender(t, "/")
	feuxNoFeedback(t, page)
	feuxHas(t, page, `<header class="topbar">`)
	feuxHas(t, page, `<a class="origin-link" href="/"`)
	feuxHas(t, page, `>http://127.0.0.1:8787/</a>`)
	head := strings.Index(page, `<header class="topbar">`)
	link := strings.Index(page, `class="origin-link"`)
	end := strings.Index(page, `</header>`)
	if !(head >= 0 && head < link && link < end) {
		t.Fatalf("origin link outside topbar: head=%d link=%d end=%d", head, link, end)
	}
	css := feuxFile(t, "style.css")
	feuxHas(t, css, `body:has(.sheet-backdrop:target) .origin-link`)
	feuxHas(t, css, `position: fixed`)
	feuxHas(t, css, `z-index: calc(var(--z-modal) + 2)`)
	// Sheets themselves must stay sheet-only (no home topbar duplication).
	sheet := feuxRender(t, "/ui/prompts")
	if strings.Contains(sheet, `class="topbar"`) {
		t.Error("prompts sheet rendered the home topbar")
	}
}

// Toasts: a real close button, a click-through container, <=4s total, and a
// short CSS fade so the linter's animation-duration rule stays satisfied.
func TestFEUXToastCloseAndShortTimeout(t *testing.T) {
	html := feuxFile(t, "index.html")
	feuxHas(t, html, `<button class="toast-close" type="button" aria-label="Dismiss notification">`)
	feuxHas(t, html, `data-toast-ms="1500"`)
	feuxHas(t, html, `style="--ms:{{.MsgMs}}ms"`)
	feuxHas(t, html, `e.preventDefault(); e.stopPropagation(); t.remove();`)
	feuxHas(t, html, `setTimeout(function () { t.remove(); }, ms);`)
	css := feuxFile(t, "style.css")
	feuxHas(t, css, `pointer-events: none;`)
	feuxHas(t, css, `pointer-events: auto;`)
	feuxHas(t, css, `animation-name: toast-out;`)
	feuxHas(t, css, `animation-duration: 220ms;`)
	feuxHas(t, css, `animation-delay: 1280ms;`)
	for _, bad := range []string{`animation: toast-out var(--ms`, `0%, 88%`} {
		if strings.Contains(css, bad) {
			t.Errorf("stale long toast rule %q remains", bad)
		}
	}
}

// SYSTEM PROMPTS hover must settle on the first frame; focus-visible stays.
func TestFEUXNavHoverNoDelay(t *testing.T) {
	css := feuxFile(t, "style.css")
	start := strings.Index(css, `.topbar-link {`)
	if start < 0 {
		t.Fatal("no .topbar-link rule")
	}
	end := strings.Index(css[start:], `}`)
	if end < 0 {
		t.Fatal("unterminated .topbar-link rule")
	}
	block := css[start : start+end]
	feuxHas(t, block, `transition: none;`)
	if strings.Contains(block, "transition-delay") {
		t.Error(".topbar-link has a transition-delay")
	}
	feuxHas(t, css, `.topbar-link:hover, .topbar-link:focus-visible`)
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

// On a 375px sheet the fixed origin link sits over the sheet title, so the
// mobile sheet starts below it and stays within the viewport.
func TestFEUXMobileSheetOffsetForOriginLink(t *testing.T) {
	css := feuxFile(t, "style.css")
	feuxHas(t, css, `body:has(.sheet-backdrop:target) .origin-link`)
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

// Dynamic fragments must keep the no-store header; no service worker or
// client cache layer may reintroduce stale lists.
func TestFEUXDynamicSheetsNoStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, target := range []string{"/", "/ui/logs", "/ui/prompts"} {
		rec := httptest.NewRecorder()
		NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, target, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", target, got)
		}
	}
	html := feuxFile(t, "index.html")
	for _, bad := range []string{"serviceWorker", "navigator.serviceWorker"} {
		if strings.Contains(html, bad) {
			t.Errorf("service worker introduced: %q", bad)
		}
	}
}
