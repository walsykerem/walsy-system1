package main

// Frontend E2E. Every test drives the real dashboard handler over loopback
// httptest with a hermetic HOME. Nothing here touches 127.0.0.1:8787, the MCP
// relays, a shipped binary or a real key. Assertions express the required
// behavior; a missing feature fails here instead of being waved through.

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	feAttrRe = regexp.MustCompile(`(?i)\b([a-z-]+)\s*=\s*"([^"]*)"`)
	feDataRe = regexp.MustCompile(`data-src="([^"]+)"`)
	feCSSRe  = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
)

// feServer starts the production handler on an ephemeral loopback port with
// isolated state. The second return is the temp HOME, for on-disk assertions.
func feServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	routerReset()
	resetLogs()
	t.Cleanup(func() { routerReset(); resetLogs() })
	srv := httptest.NewServer(NewDashboardHandler())
	t.Cleanup(srv.Close)
	return srv, home
}

// feClient never follows a redirect: the sheet target and toast message must
// stay visible to the caller. Any redirect off this loopback server is a bug.
func feClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host {
			t.Errorf("redirect escaped the dashboard: %s", req.URL)
		}
		return http.ErrUseLastResponse
	}}
}

type feResult struct {
	code   int
	body   string
	header http.Header
}

func feReq(t *testing.T, c *http.Client, method, target string, body io.Reader, ctype string) feResult {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	return feResult{resp.StatusCode, string(b), resp.Header}
}

func feGet(t *testing.T, c *http.Client, srv *httptest.Server, path string) feResult {
	t.Helper()
	return feReq(t, c, http.MethodGet, srv.URL+path, nil, "")
}

func feForm(t *testing.T, c *http.Client, srv *httptest.Server, path string, vals url.Values) feResult {
	t.Helper()
	return feReq(t, c, http.MethodPost, srv.URL+path, strings.NewReader(vals.Encode()),
		"application/x-www-form-urlencoded")
}

func feJSON(t *testing.T, c *http.Client, srv *httptest.Server, path, payload string) feResult {
	t.Helper()
	return feReq(t, c, http.MethodPost, srv.URL+path, strings.NewReader(payload), "application/json")
}

func feFlash(t *testing.T, r feResult, want string) {
	t.Helper()
	loc, err := url.Parse(r.header.Get("Location"))
	if err != nil || loc.Query().Get("msg") != want {
		t.Errorf("redirect flash = %q, want %q", r.header.Get("Location"), want)
	}
}

// feCrossOrigin proves the write endpoints reject a browser Origin mismatch.
func feCrossOrigin(t *testing.T, c *http.Client, srv *httptest.Server, path, body string) feResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return feResult{resp.StatusCode, string(b), resp.Header}
}

func feBlock(doc, open, close string) string {
	return regexp.MustCompile(`(?is)` + regexp.QuoteMeta(open) + `.*?` + regexp.QuoteMeta(close)).FindString(doc)
}

func feAttr(tag, name string) string {
	for _, m := range feAttrRe.FindAllStringSubmatch(tag, -1) {
		if strings.EqualFold(m[1], name) {
			return m[2]
		}
	}
	return ""
}

func feCSSRule(css, selector string) string {
	css = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css, "")
	for _, m := range feCSSRe.FindAllStringSubmatch(css, -1) {
		for _, sel := range strings.Split(m[1], ",") {
			if strings.TrimSpace(sel) == selector {
				return m[0]
			}
		}
	}
	return ""
}

func feHome(t *testing.T, c *http.Client, srv *httptest.Server) string {
	t.Helper()
	r := feGet(t, c, srv, "/")
	if r.code != http.StatusOK {
		t.Fatalf("GET / = %d", r.code)
	}
	return r.body
}

// The topbar carries brand and actions, never the loopback origin URL.
func TestE2EFrontendHomeNoOriginLink(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)

	topbar := feBlock(home, `<header class="topbar">`, `</header>`)
	if topbar == "" {
		t.Fatal("home has no visible topbar")
	}
	if strings.Contains(topbar, "sheet-backdrop") {
		t.Error("topbar contains a sheet/backdrop")
	}
	for _, gone := range []string{`class="origin-link"`, "http://127.0.0.1:8787/", "Dashboard origin"} {
		if strings.Contains(topbar, gone) {
			t.Errorf("topbar still renders %q", gone)
		}
	}
	if strings.Contains(feAttr(feBlock(home, "<body", ">"), "class"), "sheet-document") {
		t.Error("home document is rendered as a sheet document")
	}
}

// LTR flex nav: the right-to-left reading order LOGS, SYSTEM PROMPTS,
// LINKEDIN, GITHUB STAR ME means DOM order is its reverse.
func TestE2EFrontendTopbarRightToLeftOrder(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	nav := feBlock(feHome(t, c, srv), `<nav class="topbar-nav"`, `</nav>`)
	if nav == "" {
		t.Fatal("home has no topbar nav")
	}
	labels := []string{"GITHUB STAR ME", "LINKEDIN", "SYSTEM PROMPTS", "LOGS"}
	pos := make([]int, len(labels))
	for i, label := range labels {
		pos[i] = strings.Index(nav, label)
		if pos[i] < 0 {
			t.Errorf("topbar nav missing %q", label)
		}
	}
	for i := 1; i < len(pos); i++ {
		if pos[i] >= 0 && pos[i-1] >= 0 && pos[i] <= pos[i-1] {
			t.Errorf("right-to-left nav order broken: %q must be right of %q", labels[i], labels[i-1])
		}
	}
}

func TestE2EFrontendFeedbackWalsySystemOne(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)
	for _, want := range []string{"WALSY", "SYSTEM ONE"} {
		if !strings.Contains(home, want) {
			t.Errorf("home missing WALSY/SYSTEM ONE marker %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(home), "FEEDBACK") {
		t.Error("home still shows FEEDBACK after removal request")
	}

	r := feGet(t, c, srv, "/api/feedback")
	if r.code != http.StatusOK {
		t.Fatalf("GET /api/feedback = %d", r.code)
	}
	var fb struct {
		OK       bool   `json:"ok"`
		ReadOnly bool   `json:"readonly"`
		SOMode   string `json:"so_mode"`
	}
	if err := json.Unmarshal([]byte(r.body), &fb); err != nil {
		t.Fatalf("feedback JSON: %v (%s)", err, r.body)
	}
	if !fb.OK || !fb.ReadOnly || fb.SOMode == "" {
		t.Errorf("feedback contract not read-only: %+v", fb)
	}

	for _, path := range []string{"/api/feedback", "/api/feedback/mutation", "/ui/feedback"} {
		r := feJSON(t, c, srv, path, `{"feedback":"e2e"}`)
		if r.code != http.StatusForbidden || !strings.Contains(r.body, "MUTATION TOKEN REJECT") {
			t.Errorf("POST %s = %d %s, want 403 MUTATION TOKEN REJECT", path, r.code, r.body)
		}
	}
}

func TestE2EFrontendSROnlyStatusAndClipCSS(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)
	for _, want := range []string{
		`class="mode-banner sr-only"`, `class="so-status sr-only"`,
		"SO -32001 DENIAL ALARM", "SO HEALTH:", "MUTATION TOKEN REJECT",
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home missing sr-only status marker %q", want)
		}
	}

	css := feGet(t, c, srv, "/style.css")
	if css.code != http.StatusOK {
		t.Fatalf("GET /style.css = %d", css.code)
	}
	rule := feCSSRule(css.body, ".sr-only")
	if rule == "" {
		t.Fatal(".sr-only rule missing from stylesheet")
	}
	for _, want := range []string{"width: 1px", "height: 1px", "clip: rect(0, 0, 0, 0)"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".sr-only rule missing %q: %s", want, rule)
		}
	}
	if !strings.Contains(css.body, ".sheet-backdrop:not(:target)") ||
		!strings.Contains(css.body, "display: none !important") {
		t.Error("sheets are not hidden until :target")
	}
}

func TestE2EFrontendDataSrcFragments(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)

	paths := map[string]bool{}
	for _, m := range feDataRe.FindAllStringSubmatch(home, -1) {
		paths[html.UnescapeString(m[1])] = true
	}
	for _, want := range []string{
		"/ui/logs", "/ui/prompts", "/ui/combo/add/sheet", "/ui/combo/sheet",
	} {
		if !paths[want] {
			t.Errorf("home iframe missing data-src=%q", want)
		}
	}
	providerSheets := 0
	for p := range paths {
		if strings.HasPrefix(p, "/ui/providers/sheet?name=") {
			providerSheets++
		}
	}
	if providerSheets == 0 {
		t.Error("home has no provider sheet data-src")
	}
	if n := strings.Count(home, `src="about:blank"`); n < len(paths) {
		t.Errorf("only %d inert iframes for %d lazy sheet URLs", n, len(paths))
	}

	for p := range paths {
		u, err := url.Parse(p)
		if err != nil {
			t.Fatalf("bad data-src %q: %v", p, err)
		}
		r := feGet(t, c, srv, u.RequestURI())
		if r.code != http.StatusOK {
			t.Errorf("GET data-src %q = %d", p, r.code)
		}
		if !strings.Contains(r.header.Get("Cache-Control"), "no-store") {
			t.Errorf("GET data-src %q missing Cache-Control: no-store", p)
		}
	}
}

func TestE2EFrontendCacheControlNoStore(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	for _, p := range []string{
		"/", "/style.css", "/favicon.ico", "/ui/logs", "/ui/prompts",
		"/ui/combo/sheet", "/ui/combo/add/sheet", "/api/logs",
		"/api/prompts", "/api/feedback", "/api/providers",
	} {
		r := feGet(t, c, srv, p)
		if !strings.Contains(r.header.Get("Cache-Control"), "no-store") {
			t.Errorf("GET %s Cache-Control=%q, want no-store", p, r.header.Get("Cache-Control"))
		}
	}
}

func TestE2EFrontendPromptsTaskBoardAndJSONAdd(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)

	r := feGet(t, c, srv, "/ui/prompts")
	if r.code != http.StatusOK {
		t.Fatalf("GET /ui/prompts = %d", r.code)
	}
	for _, want := range []string{"TASK BOARD", "SAVED PROMPTS", "ADD PROMPT"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/ui/prompts missing %q", want)
		}
	}

	form := feForm(t, c, srv, "/ui/prompts/add", url.Values{
		"name": {"E2E-FORM"}, "text": {"form task body"},
	})
	if form.code != http.StatusSeeOther {
		t.Fatalf("POST /ui/prompts/add = %d %q, want 303", form.code, form.header.Get("Location"))
	}
	feFlash(t, form, "PROMPT SAVED")
	shown := feGet(t, c, srv, "/ui/prompts?msg=PROMPT+SAVED")
	if !strings.Contains(shown.body, "E2E-FORM") || !strings.Contains(shown.body, "toast-ok") {
		t.Error("saved prompt or its success toast not rendered")
	}
	removed := feForm(t, c, srv, "/ui/prompts/remove", url.Values{"name": {"E2E-FORM"}})
	if removed.code != http.StatusSeeOther {
		t.Errorf("POST /ui/prompts/remove = %d %q", removed.code, removed.header.Get("Location"))
	}
	feFlash(t, removed, "PROMPT REMOVED")

	added := feJSON(t, c, srv, "/api/prompts/add", `{"name":"E2E-JSON","text":"json task body"}`)
	if added.code != http.StatusOK || !strings.Contains(added.body, `"ok":true`) {
		t.Fatalf("POST /api/prompts/add = %d %s, want 200 JSON ok", added.code, added.body)
	}
	if !strings.Contains(added.header.Get("Cache-Control"), "no-store") {
		t.Error("POST /api/prompts/add missing Cache-Control: no-store")
	}
	list := feGet(t, c, srv, "/api/prompts")
	if list.code != http.StatusOK || !strings.Contains(list.body, "E2E-JSON") {
		t.Errorf("GET /api/prompts = %d %s, missing JSON-added prompt", list.code, list.body)
	}

	for _, bad := range []string{
		`{"name":"","text":"x"}`,
		`{"name":"x","text":""}`,
		`{"text":"no name"}`,
		`{"name":"E2E-BIG","text":"` + strings.Repeat("x", 8193) + `"}`,
	} {
		r := feJSON(t, c, srv, "/api/prompts/add", bad)
		if r.code != http.StatusBadRequest {
			t.Errorf("POST /api/prompts/add %s = %d, want 400", bad[:min(len(bad), 40)], r.code)
		}
	}
	xo := feCrossOrigin(t, c, srv, "/api/prompts/add", `{"name":"E2E-XO","text":"cross origin"}`)
	if xo.code != http.StatusBadRequest {
		t.Errorf("cross-origin POST /api/prompts/add = %d, want 400", xo.code)
	}
}

func TestE2EFrontendLogsClearAndToast(t *testing.T) {
	srv, home := feServer(t)
	c := feClient(t)

	empty := feGet(t, c, srv, "/ui/logs")
	if empty.code != http.StatusOK {
		t.Fatalf("GET /ui/logs = %d", empty.code)
	}
	for _, want := range []string{
		"LOGS", "NO CALLS YET.", "CALL(S) SINCE", "SHOWING AT MOST 200 ROWS.",
	} {
		if !strings.Contains(empty.body, want) {
			t.Errorf("/ui/logs missing %q", want)
		}
	}
	controls := regexp.MustCompile(`(?is)<(?:form|button|a)\b[^>]*(?:action="[^"]*(?:clear|delete)[^"]*"|href="[^"]*(?:clear|delete)[^"]*"|>(?:CLEAR|DELETE)[^<]*<)`)
	if !controls.MatchString(empty.body) {
		t.Error("/ui/logs has no visible clear/delete control")
	}

	resetLogs()
	logCall("systemone", "OK", 2*time.Millisecond, 11, 22, nil)
	filled := feGet(t, c, srv, "/ui/logs")
	if !strings.Contains(filled.body, "systemone") || !strings.Contains(filled.body, "CALL(S) SINCE") {
		t.Error("real logged call not shown in /ui/logs")
	}
	api := feGet(t, c, srv, "/api/logs")
	if api.code != http.StatusOK || !strings.Contains(api.body, "systemone") {
		t.Errorf("GET /api/logs = %d %s, missing real call", api.code, api.body)
	}

	cleared := feReq(t, c, http.MethodPost, srv.URL+"/api/logs/clear", nil, "")
	if cleared.code != http.StatusOK || !strings.Contains(cleared.body, `"ok":true`) {
		t.Fatalf("POST /api/logs/clear = %d %s, want 200 JSON ok", cleared.code, cleared.body)
	}
	if !strings.Contains(cleared.header.Get("Cache-Control"), "no-store") {
		t.Error("POST /api/logs/clear missing Cache-Control: no-store")
	}
	xo := feCrossOrigin(t, c, srv, "/api/logs/clear", "")
	if xo.code != http.StatusBadRequest {
		t.Errorf("cross-origin POST /api/logs/clear = %d, want 400", xo.code)
	}
	after := feGet(t, c, srv, "/ui/logs")
	if !strings.Contains(after.body, "NO CALLS YET.") {
		t.Error("logs still shown after clear")
	}
	apiAfter := feGet(t, c, srv, "/api/logs")
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(apiAfter.body), &rows); err != nil || len(rows) != 0 {
		t.Errorf("GET /api/logs after clear = %d %s, want empty array", apiAfter.code, apiAfter.body)
	}
	audit, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read audit after clear: %v", err)
	}
	if strings.Contains(string(audit), "systemone") {
		t.Error("audit.jsonl survived /api/logs/clear")
	}
	toast := feGet(t, c, srv, "/ui/logs?msg=LOGS+CLEARED")
	if !strings.Contains(toast.body, `class="toast toast-ok"`) {
		t.Error("log-clear notification not rendered")
	}
}

func TestE2EFrontendToastContract(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)

	ok := feGet(t, c, srv, "/?msg=PROMPT+SAVED&ms=500")
	if !strings.Contains(ok.body, `class="toasts" role="status" aria-live="polite"`) {
		t.Error("toast container missing status/live region")
	}
	if !strings.Contains(ok.body, `class="toast toast-ok"`) ||
		!strings.Contains(ok.body, `class="toast-close"`) {
		t.Error("success toast or its dismiss button missing")
	}
	if n := strings.Count(ok.body, `class="toast-close"`); n < 1 {
		t.Errorf("toast close button count = %d, want >= 1", n)
	}
	// A toast is a live message, not a trap: dismiss must be clickable and the
	// visible lifetime must stay bounded even when ?ms= asks for a long delay.
	if n := strings.Count(ok.body, `data-toast-ms="`); n != 1 {
		t.Errorf("data-toast-ms count = %d, want 1", n)
	}
	css := feGet(t, c, srv, "/style.css").body
	closeRule := feCSSRule(css, ".toast-close")
	if closeRule == "" || !strings.Contains(closeRule, "pointer-events: auto") {
		t.Errorf(".toast-close must re-enable pointer events: %q", closeRule)
	}
	for _, p := range []string{"/?msg=INVALID+FIELD", "/?msg=PROMPT+SAVED&ms=30000"} {
		r := feGet(t, c, srv, p)
		ms := regexp.MustCompile(`data-toast-ms="(\d+)"`).FindStringSubmatch(r.body)
		if len(ms) != 2 {
			t.Errorf("%s has no data-toast-ms", p)
			continue
		}
		var value int
		if _, err := fmt.Sscan(ms[1], &value); err != nil || value <= 0 || value > 4000 {
			t.Errorf("%s toast lifetime %sms, want 1..4000ms", p, ms[1])
		}
		if !strings.Contains(r.body, `class="toast toast-error"`) && strings.Contains(p, "INVALID") {
			t.Error("refusal toast missing error class")
		}
	}
	if !strings.Contains(css, "@keyframes toast-out") {
		t.Error("toast auto-dismiss keyframes missing")
	}
}

// UX lint contract: hover state changes on the header are instant (either no
// transition or an explicit 0s/none), and any toast fade is bounded to 150-400ms.
func TestE2EFrontendSystemPromptsTransitionDelay(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	css := feGet(t, c, srv, "/style.css").body

	instantHover := false
	slowTransition := false
	shortAnimation := false
	dur := regexp.MustCompile(`(?i)\b([0-9]+(?:\.[0-9]+)?)ms\b|\b([0-9]+(?:\.[0-9]+)?)s\b`)
	for _, m := range feCSSRe.FindAllStringSubmatch(css, -1) {
		sel, body := m[1], m[2]
		lower := strings.ToLower(body)
		hoverRule := strings.Contains(sel, "topbar-link") || strings.Contains(sel, "prompts")
		if hoverRule {
			if strings.Contains(lower, "transition: none") ||
				strings.Contains(lower, "transition-delay: 0s") ||
				strings.Contains(lower, "transition-delay: 0ms") ||
				(!strings.Contains(lower, "transition:") && !strings.Contains(lower, "transition-delay")) {
				instantHover = true
			}
			if strings.Contains(lower, "transition-delay") {
				for _, d := range dur.FindAllStringSubmatch(body, -1) {
					value := 0.0
					if d[1] != "" {
						fmt.Sscan(d[1], &value)
					} else if d[2] != "" {
						fmt.Sscan(d[2], &value)
						value *= 1000
					}
					if value > 0 {
						slowTransition = true
					}
				}
			}
		}
		toastRule := strings.Contains(sel, "toast") || strings.Contains(sel, "prompts")
		if !toastRule || (!strings.Contains(body, "animation:") && !strings.Contains(body, "animation-duration:")) {
			continue
		}
		for _, d := range dur.FindAllStringSubmatch(body, -1) {
			value := 0.0
			if d[1] != "" {
				fmt.Sscan(d[1], &value)
			} else if d[2] != "" {
				fmt.Sscan(d[2], &value)
				value *= 1000
			}
			if value >= 150 && value <= 400 {
				shortAnimation = true
			}
		}
	}
	if !instantHover {
		t.Error("no instant-transition rule for a toast/SYSTEM PROMPTS control")
	}
	if slowTransition {
		t.Error("positive transition-delay on a toast/SYSTEM PROMPTS rule")
	}
	if !shortAnimation {
		t.Error("no 150-400ms toast animation; ux-skill animation-duration-too-long")
	}
}

// Sheet targets and close anchors survive every open/close cycle.
func TestE2EFrontendSheetCloseTargets(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)

	for _, target := range []string{"#providers", "#prompts", "#logs", "#combo-add"} {
		if !strings.Contains(home, `href="`+target+`"`) {
			t.Errorf("home missing %s target link", target)
		}
	}
	if n := strings.Count(home, `class="sheet-close" href="#providers"`); n < 3 {
		t.Errorf("sheet X close anchors = %d, want >= 3", n)
	}
	if strings.Contains(home, `class="sheet-close toast-close"`) {
		t.Error("sheet close confused with toast close")
	}
	if !strings.Contains(home, `'Escape'`) || !strings.Contains(home, "'hashchange'") {
		t.Error("Escape/hashchange sheet close handler missing")
	}
}

// A sheet is a real modal: hashchange marks the background inert, and closing
// back to #providers clears it so the dashboard is interactive again.
func TestE2EFrontendSheetBackgroundInert(t *testing.T) {
	srv, _ := feServer(t)
	c := feClient(t)
	home := feHome(t, c, srv)

	start := strings.Index(home, "addEventListener('hashchange', function () {")
	if start < 0 {
		t.Fatal("home has no hashchange handler")
	}
	script := home[start:]
	if end := strings.Index(script, "</script>"); end >= 0 {
		script = script[:end]
	}
	handler := feBlock(script, "addEventListener('hashchange', function () {", "});")
	if handler == "" {
		t.Fatal("hashchange handler body missing")
	}
	if !strings.Contains(handler, "setBackgroundInert(open)") {
		t.Error("hashchange does not toggle background inert")
	}

	fn := feBlock(home, "function setBackgroundInert(open) {", "\n    }")
	if fn == "" {
		t.Fatal("setBackgroundInert helper missing")
	}
	for _, want := range []string{
		`querySelectorAll('header.topbar, main.grid > *')`,
		`closest('.sheet-backdrop')`,
		`setAttribute('inert', '')`,
		`removeAttribute('inert')`,
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("setBackgroundInert missing %q", want)
		}
	}
}

func TestE2EFrontendSecretsNeverPrinted(t *testing.T) {
	srv, home := feServer(t)
	c := feClient(t)
	const secret = "tok_e2e_SECRET_SENTINEL_9f3a"

	r := feForm(t, c, srv, "/ui/providers/keys", url.Values{
		"name": {"Evren System One"}, "action": {"add"}, "key": {secret},
	})
	if r.code != http.StatusSeeOther {
		t.Fatalf("add key = %d %q, want 303", r.code, r.header.Get("Location"))
	}
	feFlash(t, r, "KEY ADDED")

	targets := map[string]string{
		"home":     feHome(t, c, srv),
		"sheet":    feGet(t, c, srv, "/ui/providers/sheet?name="+url.QueryEscape("Evren System One")).body,
		"api":      feGet(t, c, srv, "/api/providers").body,
		"api/logs": feGet(t, c, srv, "/api/logs").body,
	}
	audit, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read audit: %v", err)
	}
	targets["audit"] = string(audit)
	for name, body := range targets {
		if strings.Contains(body, secret) {
			t.Errorf("%s leaked the stored key value", name)
		}
	}
	if !strings.Contains(targets["home"], "KEY SET") {
		t.Error("home did not reflect the key write")
	}
}
