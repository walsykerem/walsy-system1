package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Acceptance script for the JS-to-Go migration. No browser, no node, no live
// System One: only loopback httptest and the real state files in a temp HOME.

const paritySecret = "sk-test-DO-NOT-LEAK-928374"

func parityServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	s := httptest.NewServer(NewDashboardHandler())
	t.Cleanup(s.Close)
	return s
}

func parityPostJSON(t *testing.T, s *httptest.Server, path string, payload any, out any) int {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.URL+path, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && json.Unmarshal(data, out) != nil {
		t.Fatalf("decode %s: %s", path, data)
	}
	return resp.StatusCode
}

func parityForm(t *testing.T, s *httptest.Server, path string, form url.Values) int {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(s.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func parityPostPage(t *testing.T, s *httptest.Server, path string, form url.Values) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(s.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", path, resp.StatusCode, data)
	}
	return string(data)
}

func parityGet(t *testing.T, s *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(s.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, data)
	}
	return string(data)
}

func parityPaths(t *testing.T) (models, combo, keys string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".walsy-system1")
	return filepath.Join(base, "models.txt"), filepath.Join(base, "combo.txt"), filepath.Join(base, "keys")
}

func parityNoSecret(t *testing.T, bodies ...string) {
	t.Helper()
	for _, b := range bodies {
		if strings.Contains(b, paritySecret) {
			t.Fatal("secret leaked into response")
		}
	}
}

// Baseline: the JSON contract app.js used before migration stays unchanged.
func TestJSParityJSONBaselineFlows(t *testing.T) {
	s := parityServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	defer up.Close()
	modelsPath, comboPath, _ := parityPaths(t)
	var out map[string]any

	if c := parityPostJSON(t, s, "/api/providers", saveProviderRequest{
		Name: "LOCAL-A", Base: up.URL, KeyEnv: "EVREN_API_KEY",
	}, &out); c != http.StatusOK {
		t.Fatalf("add provider = %d %v", c, out)
	}
	if c := parityPostJSON(t, s, "/api/providers/sheet", sheetProviderRequest{
		Name: "LOCAL-A", Key: paritySecret, Models: []string{"m1", "m2"},
	}, &out); c != http.StatusOK {
		t.Fatalf("sheet save = %d %v", c, out)
	}
	data, err := os.ReadFile(modelsPath)
	if err != nil || !strings.Contains(string(data), "LOCAL-A\tm1\n") {
		t.Fatalf("models = %q err = %v", data, err)
	}
	steps := []comboStepJSON{{Provider: "LOCAL-A", Model: "m2"}, {Provider: "LOCAL-A", Model: "m1"}}
	if c := parityPostJSON(t, s, "/api/combo", map[string]any{"steps": steps}, &out); c != http.StatusOK {
		t.Fatalf("combo save = %d %v", c, out)
	}
	saved, err := os.ReadFile(comboPath)
	if err != nil || !strings.HasPrefix(string(saved), "LOCAL-A\tm2\n") {
		t.Fatalf("combo order = %q err = %v", saved, err)
	}
	var run ComboOutcome
	if c := parityPostJSON(t, s, "/api/combo/run", comboRequest{}, &run); c != http.StatusOK {
		t.Fatalf("combo run = %d", c)
	}
	if run.Winner != "LOCAL-A/m2" {
		t.Fatalf("winner = %q, want LOCAL-A/m2", run.Winner)
	}
	parityNoSecret(t, parityGet(t, s, "/api/providers"))
}

// After migration: every app.js DOM action is a form POST and a server-rendered page.
func TestJSParityUIForms(t *testing.T) {
	s := parityServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	defer up.Close()
	modelsPath, comboPath, keysDir := parityPaths(t)

	home := parityGet(t, s, "/")
	// One inline handler is allowed: Escape clears the :target sheet. Anything
	// that loads a script file or binds a DOM event attribute is not.
	for _, bad := range []string{"/app.js", "onclick=", "onkeydown=", "<script src="} {
		if strings.Contains(home, bad) {
			t.Fatalf("HTML still depends on JS (%s)", bad)
		}
	}
	if !strings.Contains(home, "/style.css") {
		t.Fatal("style.css link missing")
	}
	for _, anchor := range []string{`href="#sheet-Evren&#43;System&#43;One"`, `id="sheet-Evren&#43;System&#43;One"`, `src="/ui/providers/sheet?name=Evren&#43;System&#43;One"`, `id="combo-result"`, `src="/ui/combo/sheet"`} {
		if !strings.Contains(home, anchor) {
			t.Fatalf("home sheet iframe missing: %s", anchor)
		}
	}
	if strings.Contains(home, "ADD API KEY") || strings.Contains(home, "CLEAR ALL KEYS") || strings.Contains(home, "/ui/combo/run") {
		t.Fatal("sheet content leaked into home document")
	}
	sheet := parityGet(t, s, "/ui/providers/sheet?name=Evren+System+One")
	for _, anchor := range []string{"ADD API KEY", "SYSTEM ONE CONFIG", "SAVE ENDPOINT", "KEY TEST"} {
		if !strings.Contains(sheet, anchor) {
			t.Fatalf("provider sheet document missing: %s", anchor)
		}
	}
	if strings.Contains(sheet, "CLEAR ALL KEYS") || strings.Contains(sheet, `formaction="/ui/providers/reset"`) {
		t.Fatal("empty sheet shows key/reset controls")
	}
	if !strings.Contains(parityGet(t, s, "/ui/combo/sheet"), `action="/ui/combo/run"`) {
		t.Fatal("combo sheet form missing")
	}

	if c := parityForm(t, s, "/ui/providers", url.Values{
		"name": {"LOCAL-A"}, "base": {up.URL}, "keyEnv": {"EVREN_API_KEY"},
	}); c != http.StatusSeeOther {
		t.Fatalf("add provider = %d", c)
	}
	for _, m := range []string{"m1", "m2"} {
		if c := parityForm(t, s, "/ui/providers/models", url.Values{
			"name": {"LOCAL-A"}, "model": {m}, "action": {"add"},
		}); c != http.StatusSeeOther {
			t.Fatalf("add model %s = %d", m, c)
		}
	}
	data, _ := os.ReadFile(modelsPath)
	if !strings.Contains(string(data), "LOCAL-A\tm1\n") || !strings.Contains(string(data), "LOCAL-A\tm2\n") {
		t.Fatalf("models after add = %q", data)
	}

	keyPath := filepath.Join(keysDir, keyFileName("LOCAL-A", t))
	for _, key := range []string{paritySecret, paritySecret + "2"} {
		if c := parityForm(t, s, "/ui/providers/keys", url.Values{
			"name": {"LOCAL-A"}, "action": {"add"}, "key": {key},
		}); c != http.StatusSeeOther {
			t.Fatalf("add key = %d", c)
		}
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil || strings.Count(string(keyData), "\n") != 1 {
		t.Fatalf("two-key save failed: %v", err)
	}
	parityNoSecret(t, parityGet(t, s, "/"))
	if c := parityForm(t, s, "/ui/providers/keys", url.Values{
		"name": {"LOCAL-A"}, "action": {"remove"}, "index": {"1"},
	}); c != http.StatusSeeOther {
		t.Fatalf("remove key = %d", c)
	}
	keyData, _ = os.ReadFile(keyPath)
	if strings.Contains(string(keyData), "\n") {
		t.Fatalf("one key should remain")
	}
	if c := parityForm(t, s, "/ui/providers/keys", url.Values{
		"name": {"LOCAL-A"}, "action": {"clear"},
	}); c != http.StatusSeeOther {
		t.Fatalf("clear keys = %d", c)
	}
	entries, _ := os.ReadDir(keysDir)
	if len(entries) != 0 {
		t.Fatalf("keys not cleared: %v", entries)
	}

	if c := parityForm(t, s, "/ui/providers/models", url.Values{
		"name": {"LOCAL-A"}, "model": {"m2"}, "action": {"remove"},
	}); c != http.StatusSeeOther {
		t.Fatalf("remove model = %d", c)
	}
	data, _ = os.ReadFile(modelsPath)
	if strings.Contains(string(data), "LOCAL-A\tm2\n") {
		t.Fatalf("model not removed: %q", data)
	}

	if c := parityForm(t, s, "/ui/combo/add", url.Values{
		"provider": {"LOCAL-A"}, "model": {"m1"},
	}); c != http.StatusSeeOther {
		t.Fatalf("combo add = %d", c)
	}
	if data, _ = os.ReadFile(comboPath); !strings.Contains(string(data), "LOCAL-A\tm1\n") {
		t.Fatalf("combo after add = %q", data)
	}
	if c := parityForm(t, s, "/ui/combo", url.Values{"action": {"remove"}, "index": {"0"}}); c != http.StatusSeeOther {
		t.Fatalf("combo remove = %d", c)
	}
	if data, _ = os.ReadFile(comboPath); strings.Contains(string(data), "LOCAL-A") {
		t.Fatalf("combo not empty: %q", data)
	}

	if c := parityForm(t, s, "/ui/combo/add", url.Values{
		"provider": {"LOCAL-A"}, "model": {"m1"},
	}); c != http.StatusSeeOther {
		t.Fatalf("combo re-add = %d", c)
	}
	runPage := parityPostPage(t, s, "/ui/combo/run", url.Values{})
	if !strings.Contains(runPage, "WINNER ON ROUND 1: LOCAL-A/m1") {
		t.Fatalf("server-rendered winner missing: %q", runPage)
	}
	if strings.Contains(runPage, "BACK") || strings.Contains(runPage, `class="topbar"`) {
		t.Fatalf("combo result must stay a bare iframe document: %q", runPage)
	}
	parityNoSecret(t, home, runPage)

	resp, err := http.Get(s.URL + "/api/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var view providersResponse
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	for _, p := range view.Providers {
		if p.Name == "LOCAL-A" {
			if p.KeySet {
				t.Fatal("keySet true after clear")
			}
			return
		}
	}
	t.Fatal("LOCAL-A missing from unchanged JSON API")
}

// Item 3+6: DOM markup and the shared enable rule must stay in sync.
func TestJSGoParityItems36(t *testing.T) {
	s := parityServer(t)
	if err := saveCustomProvider("PARITY-P", "https://example.invalid", "key"); err != nil {
		t.Fatal(err)
	}
	if err := saveModels(map[string][]string{"PARITY-P": {"m1"}}); err != nil {
		t.Fatal(err)
	}
	home := parityGet(t, s, "/") + parityGet(t, s, "/ui/providers/sheet?name=PARITY-P") + parityGet(t, s, "/ui/logs")
	for _, anchor := range []string{
		`data-test="keytest"`,
		`id="keytest-model"`,
		`id="keytest-provider"`,
		`id="system-prompt"`,
		`class="logs-layout"`,
	} {
		if !strings.Contains(home, anchor) {
			t.Fatalf("home DOM missing %q", anchor)
		}
	}
	css := parityGet(t, s, "/style.css")
	for _, anchor := range []string{`.logs-layout { display: flex`, `.system-prompt-panel { display: flex`, `#system-prompt`} {
		if !strings.Contains(css, anchor) {
			t.Fatalf("style.css missing %q", anchor)
		}
	}
	// Same condition as the inline JS function keyEnabled().
	if keytestEnabled("") || !keytestEnabled("cloudflare/clef") {
		t.Fatal("Go/JS keytest enable rule diverged")
	}
	// Inline handler contract stays unchanged.
	for _, bad := range []string{"onclick=", "onkeydown=", "<script src="} {
		if strings.Contains(home, bad) {
			t.Fatalf("inline handler appeared: %s", bad)
		}
	}
}

func TestJSParityNoSOConfigPanel(t *testing.T) {
	s := parityServer(t)
	page := parityGet(t, s, "/")
	for _, gone := range []string{`id="so-config"`, `action="/ui/so"`, "SAVE SO CONFIG"} {
		if strings.Contains(page, gone) {
			t.Fatalf("SO config control still rendered: %q", gone)
		}
	}
	lower := strings.ToLower(page)
	for _, bad := range []string{"<script src=", "onclick", "onkeydown", "javascript:"} {
		if strings.Contains(lower, bad) {
			t.Fatalf("dashboard must not load external JS or inline handlers (%s)", bad)
		}
	}
	parityNoSecret(t, page)
}
