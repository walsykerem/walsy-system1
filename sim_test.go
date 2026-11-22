package main

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var simAttrRe = regexp.MustCompile(`(href|src|action)="([^"]*)"`)
var simIDRe = regexp.MustCompile(`id="([^"]*)"`)

// simHome boots the real dashboard handler and returns a client that refuses
// to follow a redirect off the loopback origin.
func simHome(t *testing.T) (*httptest.Server, *http.Client, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	s := httptest.NewServer(NewDashboardHandler())
	t.Cleanup(s.Close)
	c := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != via[0].URL.Host {
				t.Errorf("redirect escaped the dashboard: %s", req.URL)
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	return s, c, s.URL
}

func simGet(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", u, err)
	}
	return resp.StatusCode, string(b)
}

// simPost never follows the redirect, so the sheet target and message are
// visible instead of being buried under the followed GET.
func simPost(t *testing.T, c *http.Client, s *httptest.Server, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(s.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestServedMarkupIsSelfContained walks the markup the dashboard actually
// serves and proves every link stays inside it: no external host, every
// fragment resolves to an element in the document, every same-origin URL the
// page points at answers 200 with a document rather than a JSON error.
func TestServedMarkupIsSelfContained(t *testing.T) {
	_, c, base := simHome(t)
	status, home := simGet(t, c, base+"/")
	if status != 200 {
		t.Fatalf("GET / = %d", status)
	}

	for _, gone := range []string{"LOCAL DASHBOARD", "NO TELEMETRY", "NO JAVASCRIPT", "STDLIB ONLY", `class="footer"`} {
		if strings.Contains(home, gone) {
			t.Errorf("retired chrome text still served: %q", gone)
		}
	}
	for _, want := range []string{"Command Code", `addEventListener('keydown'`, `e.key !== 'Escape'`} {
		if !strings.Contains(home, want) {
			t.Errorf("home missing %q", want)
		}
	}

	attrs := simAttrRe.FindAllStringSubmatch(home, -1)
	if len(attrs) == 0 {
		t.Fatal("home page served no href/src/action")
	}
	ids := map[string]bool{}
	for _, m := range simIDRe.FindAllStringSubmatch(home, -1) {
		ids[m[1]] = true
	}

	routes := map[string]bool{}
	for _, m := range attrs {
		kind, val := m[1], m[2]
		if val == "" {
			continue
		}
		if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") || strings.HasPrefix(val, "//") {
			t.Errorf("external %s=%q on the home page", kind, val)
			continue
		}
		if strings.HasPrefix(val, "#") {
			if kind != "href" {
				t.Errorf("%s=%q uses a fragment", kind, val)
			}
			if id := strings.TrimPrefix(val, "#"); id != "" && !ids[id] {
				t.Errorf("fragment %q has no element in the served document", val)
			}
			continue
		}
		if !strings.HasPrefix(val, "/") {
			t.Errorf("%s=%q is neither a fragment nor same-origin", kind, val)
			continue
		}
		if kind == "href" || kind == "src" {
			routes[html.UnescapeString(val)] = true
		}
	}
	if len(routes) == 0 {
		t.Fatal("no same-origin documents referenced")
	}

	paths := make([]string, 0, len(routes))
	for r := range routes {
		paths = append(paths, r)
	}
	sort.Strings(paths)
	for _, p := range paths {
		code, body := simGet(t, c, base+p)
		if code != 200 {
			t.Errorf("GET %s referenced by the page = %d", p, code)
			continue
		}
		if strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".ico") {
			continue
		}
		if !strings.Contains(body, "<html") {
			t.Errorf("GET %s served JSON/text, not a document: %.80q", p, body)
		}
		if strings.Contains(body, `"error"`) {
			t.Errorf("GET %s served an API error: %.120q", p, body)
		}
		// Sheet documents render inside the home page, so they must not carry
		// the standalone chrome (topbar brand).
		if strings.Contains(body, "WALSY<span") && strings.Contains(p, "/sheet") {
			t.Errorf("GET %s is a sheet but renders the topbar", p)
		}
	}

	// The add-to-combo control must open a sheet, not a page.
	if !strings.Contains(home, `href="#combo-add"`) {
		t.Error("home page lost the in-page + ADD TO COMBO sheet trigger")
	}
	if strings.Contains(home, `href="/ui/combo/add"`) {
		t.Error("home page still navigates to the standalone add page")
	}
	if !strings.Contains(home, `src="/ui/combo/add/sheet"`) {
		t.Error("home page has no add-to-combo sheet frame")
	}
}

// TestUserJourneySheetsAndState drives one full session through the HTTP API
// and checks the UI response, the redirect target and the persisted state move
// together. Nothing here should ever leave a sheet.
func TestUserJourneySheetsAndState(t *testing.T) {
	s, c, base := simHome(t)
	c = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	const provider = "Evren System One"

	// 1. Model add round trip: the combo only accepts models that exist.
	r, _ := simPost(t, c, s, "/ui/providers/models", url.Values{"name": {provider}, "model": {"sim/model"}, "action": {"add"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("model add status = %d", r.StatusCode)
	}
	if models, _ := loadModels(); len(models[provider]) == 0 || models[provider][len(models[provider])-1] != "sim/model" {
		t.Fatalf("model add not persisted: %v", models[provider])
	}
	_, afterAdd := simGet(t, c, base+"/ui/providers/sheet?name="+url.QueryEscape(provider))
	if !strings.Contains(afterAdd, "sim/model") {
		t.Error("provider sheet does not list the model just added")
	}

	// 2. Add a combo model from the sheet.
	resp, _ := simPost(t, c, s, "/ui/combo/add?sheet=1", url.Values{"provider": {provider}, "model": {"sim/model"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("combo add status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/ui/combo/add/sheet?msg=") {
		t.Fatalf("combo add left the sheet: Location=%q", loc)
	}
	steps, err := loadCombo()
	if err != nil || len(steps) != 1 || steps[0] != (stateStep{Provider: provider, Model: "sim/model"}) {
		t.Fatalf("combo state after add = %v, %v (Location %q)", steps, err, loc)
	}
	code, sheetBody := simGet(t, c, base+"/ui/combo/add/sheet")
	if code != 200 || !strings.Contains(sheetBody, "sim/model") {
		t.Fatalf("add sheet does not show the model it just stored (code %d)", code)
	}
	if strings.Contains(sheetBody, "WALSY<span") {
		t.Error("add sheet renders the standalone topbar")
	}
	// Duplicate add keeps the user in the same sheet and says so.
	resp, _ = simPost(t, c, s, "/ui/combo/add?sheet=1", url.Values{"provider": {provider}, "model": {"sim/model"}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "ALREADY") {
		t.Errorf("duplicate combo add message = %q", loc)
	}

	// 3. Two keys through the provider sheet must both survive.
	const k1, k2 = "sim-key-one-aaaa", "sim-key-two-bbbb"
	for _, k := range []string{k1, k2} {
		r, _ := simPost(t, c, s, "/ui/providers/keys", url.Values{"name": {provider}, "action": {"add"}, "key": {k}})
		if r.StatusCode != http.StatusSeeOther {
			t.Fatalf("key add status = %d", r.StatusCode)
		}
	}
	keys, err := loadKeys(provider)
	if err != nil || len(keys) != 2 {
		t.Fatalf("stored keys after two adds = %d, %v", len(keys), err)
	}
	// The provider sheet shows the count but never the values.
	code, providerSheet := simGet(t, c, base+"/ui/providers/sheet?name="+url.QueryEscape(provider))
	if code != 200 {
		t.Fatalf("provider sheet = %d", code)
	}
	for _, k := range []string{k1, k2} {
		if strings.Contains(providerSheet, k) {
			t.Errorf("provider sheet leaked a key value")
		}
	}
	if strings.Contains(providerSheet, "WALSY<span") {
		t.Error("provider sheet renders the standalone topbar")
	}

	// 4. Model remove is reflected in state.
	r, _ = simPost(t, c, s, "/ui/providers/models", url.Values{"name": {provider}, "model": {"sim/model"}, "action": {"remove"}})
	if models, _ := loadModels(); len(models[provider]) != 0 {
		t.Fatalf("model remove not persisted: %v", models[provider])
	}

	// 4b. Custom providers expose a remove control; removal drops every trace.
	const custom = "Sim Custom"
	r, _ = simPost(t, c, s, "/ui/providers", url.Values{"name": {custom}, "base": {"https://sim.example/v1"}, "keyEnv": {"SIM_KEY"}})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("custom provider add status = %d", r.StatusCode)
	}
	_, home := simGet(t, c, base+"/")
	if !strings.Contains(home, `value="Sim Custom"`) || !strings.Contains(home, `action="/ui/providers/remove"`) {
		t.Fatal("custom provider has no remove control on the home page")
	}
	r, _ = simPost(t, c, s, "/ui/providers/remove", url.Values{"name": {custom}})
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "PROVIDER+REMOVED") {
		t.Fatalf("remove redirect = %q", loc)
	}
	if _, home = simGet(t, c, base+"/"); strings.Contains(home, `value="Sim Custom"`) {
		t.Error("removed provider still listed")
	}

	// 5. Build target save stays on the home document.
	r, _ = simPost(t, c, s, "/ui/build-target", url.Values{"target": {"darwin/arm64"}})
	if r.StatusCode != http.StatusSeeOther && r.StatusCode != http.StatusOK {
		t.Errorf("build-target status = %d", r.StatusCode)
	}
	if data, err := os.ReadFile(filepath.Join(mustStateDir(t), "build-target.txt")); err != nil || strings.TrimSpace(string(data)) != "darwin/arm64" {
		t.Errorf("build target not persisted: %q %v", data, err)
	}
}

func mustStateDir(t *testing.T) string {
	t.Helper()
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestNoSecretInAnyServedPage walks every document the dashboard serves and
// fails if a stored key value appears anywhere in the markup.
func TestNoSecretInAnyServedPage(t *testing.T) {
	s, c, base := simHome(t)
	c = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	const provider, secret = "OpenRouter", "sk-or-simulate-secret-999"
	if r, _ := simPost(t, c, s, "/ui/providers/keys", url.Values{"name": {provider}, "action": {"add"}, "key": {secret}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("key add status = %d", r.StatusCode)
	}
	pages := []string{
		"/",
		"/ui/providers/sheet?name=" + url.QueryEscape(provider),
		"/ui/combo/add/sheet",
		"/ui/combo/sheet",
	}
	for _, p := range pages {
		code, body := simGet(t, c, base+p)
		if code != 200 {
			t.Fatalf("GET %s = %d", p, code)
		}
		if strings.Contains(body, secret) {
			t.Errorf("key value leaked into %s", p)
		}
		if strings.Contains(body, "x-api-key") || strings.Contains(body, "Bearer ") {
			t.Errorf("%s exposes a credential header name", p)
		}
	}
}

func TestServedPagesAreNotCached(t *testing.T) {
	s, c, _ := simHome(t)
	for _, path := range []string{"/", "/style.css", "/ui/combo/add/sheet", "/ui/combo/sheet"} {
		resp, err := c.Get(s.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}

func TestComboAddSheetHasNoSearchBox(t *testing.T) {
	s, c, _ := simHome(t)
	_, page := simGet(t, c, s.URL+"/ui/combo/add/sheet")
	// The shared script mentions combo-search by id; the sheet must not render the input itself.
	for _, unwanted := range []string{`id="combo-search"`, `>SEARCH<`} {
		if strings.Contains(page, unwanted) {
			t.Fatalf("combo add sheet must not render %q", unwanted)
		}
	}
	simPost(t, c, s, "/ui/providers/models", url.Values{
		"name": {"Evren System One"}, "model": {"sheet/check"}, "action": {"add"},
	})
	_, page = simGet(t, c, s.URL+"/ui/combo/add/sheet")
	if !strings.Contains(page, `action="/ui/combo/add?sheet=1"`) || !strings.Contains(page, "Evren System One/sheet/check") {
		t.Fatalf("combo add sheet lost its model buttons")
	}
}

func TestComboAddPageFiltersWhileTyping(t *testing.T) {
	s, c, _ := simHome(t)
	_, page := simGet(t, c, s.URL+"/ui/combo/add")
	for _, want := range []string{`id="combo-search"`, `addEventListener('input'`, `#combo-model-list .model-group`} {
		if !strings.Contains(page, want) {
			t.Fatalf("combo add page lost live filter: %q missing", want)
		}
	}
}

func TestSheetsCloseOnEscapeAndOutsideClick(t *testing.T) {
	s, c, _ := simHome(t)
	_, home := simGet(t, c, s.URL+"/")
	if got := strings.Count(home, `class="sheet-dismiss"`); got < 3 {
		t.Fatalf("every sheet needs a click-outside anchor, got %d", got)
	}
	for _, want := range []string{
		`href="#providers" aria-label="Close sheet" tabindex="-1"`,
		`e.key !== 'Escape'`,
		`home.location.hash = '#providers'`,
	} {
		if !strings.Contains(home, want) {
			t.Fatalf("sheet close markup missing %q", want)
		}
	}
}

func TestRemovalPrunesCombo(t *testing.T) {
	s, c, _ := simHome(t)
	simPost(t, c, s, "/ui/providers", url.Values{
		"name": {"Sim Prune"}, "base": {"https://api.example.com/v1"}, "keyEnv": {"SIM_PRUNE_KEY"},
	})
	simPost(t, c, s, "/ui/providers/models", url.Values{
		"name": {"Sim Prune"}, "model": {"m1"}, "action": {"add"},
	})
	simPost(t, c, s, "/ui/providers/models", url.Values{
		"name": {"Sim Prune"}, "model": {"m2"}, "action": {"add"},
	})
	if err := saveCombo([]stateStep{{Provider: "Sim Prune", Model: "m1"}, {Provider: "Sim Prune", Model: "m2"}}); err != nil {
		t.Fatalf("seed combo: %v", err)
	}
	simPost(t, c, s, "/ui/providers/models", url.Values{
		"name": {"Sim Prune"}, "model": {"m1"}, "action": {"remove"},
	})
	if combo, _ := loadCombo(); len(combo) != 1 || combo[0].Model != "m2" {
		t.Fatalf("removed model still in combo: %+v", combo)
	}
	simPost(t, c, s, "/ui/providers/remove", url.Values{"name": {"Sim Prune"}})
	if combo, _ := loadCombo(); len(combo) != 0 {
		t.Fatalf("removed provider still in combo: %+v", combo)
	}
}

// TestRemovalRejectsUnknownName keeps the dashboard from reporting a write for
// a provider that never existed; an orphan with models is still removable.
func TestRemovalRejectsUnknownName(t *testing.T) {
	s, c, _ := simHome(t)
	resp, _ := simPost(t, c, s, "/ui/providers/remove", url.Values{"name": {"Ghost Provider"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Request.URL.RawQuery, "INVALID+PROVIDER") {
		t.Fatalf("unknown provider removal reported success: %d %s", resp.StatusCode, resp.Request.URL)
	}
}
