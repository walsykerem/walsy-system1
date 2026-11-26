package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dashRequest builds a request with the loopback Host the dashboard requires.
func dashRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8787"
	return req
}

func dashboardTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := httptest.NewServer(NewDashboardHandler())
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return s, u.Host
}

func postJSONTest(t *testing.T, url string, payload any, v any) int {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if v != nil {
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("JSON decode failed: %v (body %q)", err, data)
		}
	}
	return resp.StatusCode
}

func postFormTest(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := dashRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDashboardHasNoDemoProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out providersResponse
	req := dashRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Providers {
		if strings.HasPrefix(p.Name, "DEMO/") {
			t.Fatalf("demo provider still seeded: %+v", p)
		}
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reg {
		if strings.HasPrefix(p.Name, "DEMO/") {
			t.Fatalf("registry contains demo provider: %s", p.Name)
		}
	}
}

func TestDashboardComboRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	if err := saveCustomProvider("LOCAL/DOWN", down.URL+"/v1", "DOWN_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveCustomProvider("LOCAL/UP", up.URL+"/v1", "UP_KEY"); err != nil {
		t.Fatal(err)
	}
	s, _ := dashboardTestServer(t)
	var out ComboOutcome
	code := postJSONTest(t, s.URL+"/api/combo/run", comboRequest{Names: []string{"LOCAL/DOWN", "LOCAL/UP"}}, &out)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out.Winner != "LOCAL/UP" {
		t.Fatalf("winner = %q, want LOCAL/UP", out.Winner)
	}
	if len(out.Steps) != 2 || !out.Steps[1].OK || out.Steps[0].OK {
		t.Fatalf("steps = %+v", out.Steps)
	}
}

func TestForgedHostCannotChangeFetchTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var fixedHits, attackerHits int
	fixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixedHits++
	}))
	defer fixed.Close()
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits++
	}))
	defer attacker.Close()
	if err := saveCustomProvider("LOCAL/FIXED", fixed.URL+"/v1", "FIXED_KEY"); err != nil {
		t.Fatal(err)
	}
	h := NewDashboardHandler()

	// A non-loopback Host is rejected before routing, so neither the provider
	// list nor a probe can be pointed at an attacker-controlled name.
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Host = "attacker.example"
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged Host status = %d, want 400", rec.Code)
	}

	body, err := json.Marshal(comboRequest{Names: []string{"LOCAL/FIXED"}})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/combo/run", strings.NewReader(string(body)))
	req.Host = strings.TrimPrefix(attacker.URL, "http://")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("combo run status = %d", rec.Code)
	}
	var run ComboOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if len(run.Steps) != 1 || run.Steps[0].Base != fixed.URL+"/v1" {
		t.Fatalf("probe base changed by forged Host: %+v", run.Steps)
	}
	if fixedHits != 1 {
		t.Fatalf("stored base probed %d times, want 1", fixedHits)
	}
	if attackerHits != 0 {
		t.Fatalf("forged Host caused %d outbound requests", attackerHits)
	}
}

func TestCrossOriginPOSTRejected(t *testing.T) {
	s, _ := dashboardTestServer(t)
	for _, path := range []string{"/api/providers", "/api/providers/sheet", "/api/combo", "/api/combo/run", "/ui/providers", "/ui/providers/endpoint", "/ui/providers/reset", "/ui/providers/sheet", "/ui/providers/keys", "/ui/providers/models", "/ui/combo", "/ui/combo/add", "/ui/combo/run", "/ui/build-target", "/ui/build"} {
		req, err := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://attacker.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s cross-origin POST: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func TestNonJSONAndOversizedRequestsRejected(t *testing.T) {
	s, _ := dashboardTestServer(t)
	req, err := http.NewRequest(http.MethodPost, s.URL+"/api/providers/sheet", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("text/plain: status = %d, want 400", resp.StatusCode)
	}
	big := strings.Repeat("x", maxRequestBytes+1)
	var out map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", map[string]any{"name": big}, &out)
	if code != http.StatusBadRequest || out["ok"] != false {
		t.Fatalf("oversized request: status = %d body = %v", code, out)
	}
}

func TestOverLimitArraysRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := dashboardTestServer(t)
	models := make([]string, maxModelsPerProvider+1)
	for i := range models {
		models[i] = "m"
	}
	var out map[string]any
	code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{
		Name: "Evren System One", Models: models,
	}, &out)
	if code != http.StatusBadRequest || out["ok"] != false {
		t.Fatalf("over-limit models: status = %d body = %v", code, out)
	}
	steps := make([]comboStepJSON, maxComboSteps+1)
	for i := range steps {
		steps[i] = comboStepJSON{Provider: "Evren System One", Model: "m"}
	}
	code = postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": steps}, &out)
	if code != http.StatusBadRequest || out["ok"] != false {
		t.Fatalf("over-limit steps: status = %d body = %v", code, out)
	}
	names := make([]string, maxComboSteps+1)
	for i := range names {
		names[i] = "Evren System One"
	}
	code = postJSONTest(t, s.URL+"/api/combo/run", comboRequest{Names: names}, &out)
	if code != http.StatusBadRequest || out["ok"] != false {
		t.Fatalf("over-limit names: status = %d body = %v", code, out)
	}
}

func TestMethodChecks(t *testing.T) {
	s, _ := dashboardTestServer(t)
	for _, path := range []string{"/api/providers/sheet", "/api/combo/run", "/ui/providers", "/ui/providers/endpoint", "/ui/providers/reset", "/ui/providers/keys", "/ui/providers/models", "/ui/combo", "/ui/build-target", "/ui/build"} {
		resp, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

func findViewProvider(t *testing.T, name string) dashboardProvider {
	t.Helper()
	view, err := providerView()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range view.Providers {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q missing from view", name)
	return dashboardProvider{}
}

func TestProviderEndpointAndResetRoutes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := NewDashboardHandler()

	// Baked override, keyEnv omitted: inherited, endpoint and default both shown.
	rec := postFormTest(t, h, "/ui/providers/endpoint", url.Values{
		"name": {"Evren System One"}, "base": {"https://custom.example/v1"},
	})
	check(t, rec.Code == http.StatusSeeOther, "endpoint save must redirect 303")
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/ui/providers/sheet?name="), "endpoint save stays in sheet")
	check(t, strings.Contains(rec.Header().Get("Location"), "msg=ENDPOINT+SAVED"), "ENDPOINT SAVED")
	evren := findViewProvider(t, "Evren System One")
	check(t, evren.Base == "https://custom.example/v1", "endpoint overridden")
	check(t, evren.KeyEnv == "EVREN_API_KEY", "baked key env inherited")
	check(t, evren.DefaultBase == "https://evren-llmapi.ssyz.org.tr/v1/systemone", "baked default retained")
	check(t, evren.CanReset, "override is resettable")

	// New custom provider with explicit keyEnv.
	rec = postFormTest(t, h, "/ui/providers/endpoint", url.Values{
		"name": {"My API"}, "base": {"http://127.0.0.1:9999/v1"}, "keyEnv": {"MY_KEY"},
	})
	check(t, rec.Code == http.StatusSeeOther, "custom endpoint redirect")
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/ui/providers/sheet?name="), "custom endpoint stays in sheet")
	reg, err := loadRegistry()
	check(t, err == nil, "registry reload")
	check(t, len(reg) == len(bakedProviders)+1, "baked + 1 custom, baked override replaces in place")
	check(t, !findViewProvider(t, "My API").CanReset, "custom has no baked default to reset to")

	// Invalid base: generic message, raw base never echoed, file untouched.
	regPath := filepath.Join(home, ".walsy-system1", "providers.txt")
	before, err := os.ReadFile(regPath)
	check(t, err == nil, "registry readable")
	rec = postFormTest(t, h, "/ui/providers/endpoint", url.Values{
		"name": {"Bad"}, "base": {"http://example.com"}, "keyEnv": {"K"},
	})
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/ui/providers/sheet?name="), "rejected endpoint stays in sheet")
	check(t, strings.Contains(rec.Header().Get("Location"), "msg=ENDPOINT+REJECTED"), "invalid base rejected")
	check(t, !strings.Contains(rec.Body.String(), "http://example.com"), "raw base not echoed")
	after, err := os.ReadFile(regPath)
	check(t, err == nil, "registry still readable")
	check(t, string(before) == string(after), "rejected endpoint must not write")

	// Reset baked override: default endpoint returns.
	rec = postFormTest(t, h, "/ui/providers/reset", url.Values{"name": {"Evren System One"}})
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/ui/providers/sheet?name="), "baked reset stays in sheet")
	check(t, strings.Contains(rec.Header().Get("Location"), "msg=ENDPOINT+RESET"), "ENDPOINT RESET")
	evren = findViewProvider(t, "Evren System One")
	check(t, evren.Base == bakedBase("Evren System One") && !evren.CanReset, "baked default restored")

	// Untouched baked provider keeps its documented endpoint.
	openRouter := findViewProvider(t, "OpenRouter")
	check(t, !openRouter.CanReset && openRouter.Base == "https://openrouter.ai/api/v1/systemone", "baked endpoint shown")

	// Remove a custom provider; its stored models return when re-added.
	models, err := loadModels()
	check(t, err == nil, "models load")
	models["My API"] = []string{"m1"}
	check(t, saveModels(models) == nil, "models saved")
	// A custom slot has no baked default, so RESET refuses instead of deleting
	// the provider (which used to leave its stored key orphaned on disk).
	rec = postFormTest(t, h, "/ui/providers/reset", url.Values{"name": {"My API"}})
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/ui/providers/sheet?name="), "custom reset stays in sheet")
	check(t, strings.Contains(rec.Header().Get("Location"), "msg=NO+DEFAULT+ENDPOINT+TO+RESET"), "custom reset refused")
	reg, err = loadRegistry()
	check(t, err == nil, "registry reload")
	check(t, len(reg) == len(bakedProviders)+1, "custom kept, baked list intact")
	added := findViewProvider(t, "My API")
	check(t, len(added.Models) == 1 && added.Models[0] == "m1", "models untouched by the refused reset")
}

func TestSaveCustomProviderValidAndRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".walsy-system1")
	regPath := filepath.Join(dir, "providers.txt")
	s, _ := dashboardTestServer(t)

	var okResp map[string]any
	code := postJSONTest(t, s.URL+"/api/providers", saveProviderRequest{
		Name: "LOCAL-A", Base: "http://127.0.0.1:8787/v1", KeyEnv: "EVREN_API_KEY",
	}, &okResp)
	if code != http.StatusOK || okResp["ok"] != true {
		t.Fatalf("valid save: status = %d body = %v", code, okResp)
	}
	st, err := os.Lstat(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("registry mode = %o, want 600", st.Mode().Perm())
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range reg {
		if p.Name == "LOCAL-A" {
			found = true
		}
	}
	if !found {
		t.Fatal("saved provider missing from reloaded registry")
	}

	before, _ := os.ReadFile(regPath)
	var errResp map[string]any
	code = postJSONTest(t, s.URL+"/api/providers", saveProviderRequest{
		Name: "BAD", Base: "http://example.com", KeyEnv: "K",
	}, &errResp)
	if code != http.StatusOK && code != http.StatusBadRequest {
		t.Fatalf("rejected save status = %d", code)
	}
	if errResp["ok"] != false {
		t.Fatalf("non-loopback save should report ok=false: %v", errResp)
	}
	after, _ := os.ReadFile(regPath)
	if string(before) != string(after) {
		t.Fatal("registry written despite rejected base")
	}
}

func TestSaveCustomProviderBadModeRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(dir, "providers.txt")
	if err := os.WriteFile(regPath, []byte("LOCAL-B,http://127.0.0.1:8787,K\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRegistryFrom(regPath); err == nil {
		t.Fatal("registry with mode 0644 must be rejected")
	}
}

func TestUISheetRoutes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := NewDashboardHandler()

	get := func(path string) *httptest.ResponseRecorder {
		req := dashRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/ui/providers/sheet?name=Evren+System+One")
	check(t, rec.Code == http.StatusOK, "provider sheet GET must render")
	check(t, strings.Contains(rec.Body.String(), "Evren System One"), "sheet shows provider name")

	rec = get("/ui/providers/sheet?name=MISSING")
	check(t, rec.Code == http.StatusSeeOther, "unknown provider redirects")
	check(t, strings.HasPrefix(rec.Header().Get("Location"), "/?msg=INVALID+PROVIDER"), "unknown provider bounced home")

	rec = get("/ui/combo/sheet")
	check(t, rec.Code == http.StatusOK, "combo sheet GET must render")

	rec = get("/ui/combo/run")
	check(t, rec.Code == http.StatusBadRequest, "combo run GET is not a page")

	rec = postFormTest(t, h, "/ui/combo/run", url.Values{})
	check(t, rec.Code == http.StatusOK, "combo run POST renders in-sheet")
}

func TestDashboardLoopbackAliasesAllowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	for _, host := range []string{"127.0.0.1:8787", "localhost:8787", "[::1]:8787", "127.0.0.1", "[::1]"} {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d, want 200", host, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/combo", strings.NewReader(`{"steps":[]}`))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8787")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin loopback: status = %d, want 200", rec.Code)
	}
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:9999", "http://localhost"} {
		req = httptest.NewRequest(http.MethodPost, "/api/combo", strings.NewReader(`{"steps":[]}`))
		req.Host = "127.0.0.1:8787"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("other loopback origin %q: status = %d, want 400", origin, rec.Code)
		}
	}
}

func TestProviderSheetJSONWithoutModelsPreservesList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := dashboardTestServer(t)
	const name = "Evren System One"
	var out map[string]any
	if code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{Name: name, Models: []string{"kept/model"}}, &out); code != http.StatusOK {
		t.Fatalf("seed models: status = %d body = %v", code, out)
	}
	if code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{Name: name, Key: "sk-kept"}, &out); code != http.StatusOK {
		t.Fatalf("key-only save: status = %d body = %v", code, out)
	}
	all, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[name]; len(got) != 1 || got[0] != "kept/model" {
		t.Fatalf("models after key-only save = %v, want [kept/model]", got)
	}
}

func TestProviderSheetAppendsKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := dashboardTestServer(t)
	const name = "Evren System One"
	var out map[string]any
	for _, key := range []string{"key-one", "key-two"} {
		if code := postJSONTest(t, s.URL+"/api/providers/sheet", sheetProviderRequest{Name: name, Key: key}, &out); code != http.StatusOK {
			t.Fatalf("key %q: status = %d body = %v", key, code, out)
		}
	}
	keys, err := loadKeys(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "key-one" || keys[1] != "key-two" {
		t.Fatalf("keys = %v, want both slots", keys)
	}
}

func TestParallelModelAddsAllPersist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := dashboardTestServer(t)
	const name = "Evren System One"
	const n = 8
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			form := url.Values{"name": {name}, "action": {"add"}, "model": {fmt.Sprintf("m%d", i)}}.Encode()
			resp, err := client.Post(s.URL+"/ui/providers/models", "application/x-www-form-urlencoded", strings.NewReader(form))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Errorf("model %d: status = %d", i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	models, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if list := models[name]; len(list) != n {
		t.Fatalf("models persisted = %d (%v), want %d", len(list), list, n)
	}
}

func TestProviderRemoveRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()

	rec := postFormTest(t, h, "/ui/providers/remove", url.Values{"name": {"Evren System One"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "BUILT+IN+PROVIDER") {
		t.Fatalf("baked remove: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	if err := saveCustomProvider("Temp Co", "https://temp.example/v1", "TEMP_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveKey("Temp Co", "sk-temp-secret"); err != nil {
		t.Fatal(err)
	}
	if err := saveModels(map[string][]string{"Temp Co": {"m1"}}); err != nil {
		t.Fatal(err)
	}

	rec = postFormTest(t, h, "/ui/providers/remove", url.Values{"name": {"Temp Co"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "PROVIDER+REMOVED") {
		t.Fatalf("custom remove: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reg {
		if p.Name == "Temp Co" {
			t.Fatal("registry entry survived removal")
		}
	}
	models, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := models["Temp Co"]; ok {
		t.Fatal("models survived removal")
	}
	path, err := keyFilePath("Temp Co")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("key file survived removal: %v", err)
	}
}

// Weight is part of the combo contract (VERIFY.md): it must round-trip through
// the JSON API, persist, and stay out of the file when unset.
func TestComboWeightRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // never write the real ~/.walsy-system1/combo.txt
	s, _ := dashboardTestServer(t)
	var out map[string]any
	steps := []comboStepJSON{
		{Provider: "Evren System One", Model: "a", Weight: 1},
		{Provider: "Evren System One", Model: "b", Weight: 10},
	}
	if code := postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": steps}, &out); code != http.StatusOK {
		t.Fatalf("save: status = %d body = %v", code, out)
	}
	var got struct {
		Steps []comboStepJSON `json:"steps"`
	}
	if code := getJSONTest(t, s.URL+"/api/combo", &got); code != http.StatusOK {
		t.Fatalf("get: status = %d", code)
	}
	if len(got.Steps) != 2 || got.Steps[1].Weight != 10 || got.Steps[0].Weight != 1 {
		t.Fatalf("weights lost: %+v", got.Steps)
	}
	stored, err := loadCombo()
	if err != nil || len(stored) != 2 || stored[1].Weight != 10 {
		t.Fatalf("weights not persisted: %+v, %v", stored, err)
	}
	order := comboOrder(toComboSteps(providersResponse{}, stored))
	if len(order) != 2 || order[0] != 1 {
		t.Fatalf("weights do not order the combo: %v", order)
	}

	if code := postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": []comboStepJSON{{Provider: "Evren System One", Model: "c"}}}, &out); code != http.StatusOK {
		t.Fatalf("save unweighted: status = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(stateDirOrFatal(t), "combo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\t0") || strings.Contains(string(data), "\t\t") {
		t.Fatalf("unweighted step wrote a third column: %q", data)
	}
}

func stateDirOrFatal(t *testing.T) string {
	t.Helper()
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// A browser must not get the JSON error shape, and a read-only route must not
// answer a write method.
func TestDashNotFoundAndHealthMethod(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()

	page := dashRequest(http.MethodGet, "/nope", nil)
	page.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, page)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "BACK TO DASHBOARD") {
		t.Fatalf("browser 404 = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("browser 404 Cache-Control = %q", got)
	}

	api := dashRequest(http.MethodGet, "/nope", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, api)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not found"`) {
		t.Fatalf("api 404 = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, dashRequest(http.MethodPost, "/api/health", nil))
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("POST /api/health = %d %q", rec.Code, rec.Body.String())
	}
}

// Malformed JSON on /api/release must answer 400, not 200 empty body.
func TestDashboardReleaseMalformedJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	req := dashRequest(http.MethodPost, "/api/release", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed /api/release status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("malformed /api/release body missing error envelope: %q", rec.Body.String())
	}
}
