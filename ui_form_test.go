package main

import (
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func addProviderForm(name, base, keyEnv string) url.Values {
	return url.Values{"name": {name}, "base": {base}, "keyEnv": {keyEnv}}
}

// Every add-provider outcome must redirect with an uppercase msg the frontend
// can show; a failure must never be a silent redirect or a bare non-2xx.
func TestUIAddProviderMessages(t *testing.T) {
	cases := []struct {
		label string
		form  url.Values
		want  string
	}{
		{"valid", addProviderForm("ACME", "https://api.example.com/v1", "ACME_KEY"), "PROVIDER SAVED"},
		{"empty name", addProviderForm("", "https://api.example.com/v1", "K"), "PROVIDER SAVE FAILED: NAME REQUIRED"},
		{"empty base", addProviderForm("ACME", "", "K"), "PROVIDER SAVE FAILED: BASE REQUIRED"},
		{"empty key env", addProviderForm("ACME", "https://api.example.com/v1", ""), "PROVIDER SAVE FAILED: KEY ENV REQUIRED"},
		{"demo name", addProviderForm("DEMO/OK", "https://api.example.com/v1", "K"), "PROVIDER SAVE FAILED: NAME RESERVED"},
		{"comma in name", addProviderForm("Bad,Provider", "https://api.example.com/v1", "K"), "PROVIDER SAVE FAILED: FIELD HAS SEPARATOR"},
		{"newline in name", addProviderForm("Bad\nName", "https://api.example.com/v1", "K"), "PROVIDER SAVE FAILED: FIELD HAS SEPARATOR"},
		{"missing scheme", addProviderForm("ACME", "api.example.com/v1", "K"), "PROVIDER SAVE FAILED: BASE REJECTED: NEEDS A FULL URL WITH HTTPS:// AND A HOST"},
		{"remote http", addProviderForm("ACME", "http://remote.example.com/v1", "K"), "PROVIDER SAVE FAILED: BASE REJECTED: PLAIN HTTP ONLY FOR 127.0.0.1 OR LOCALHOST, USE HTTPS:// HERE"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			rec := postFormTest(t, NewDashboardHandler(), "/ui/providers", tc.form)
			if rec.Code == http.StatusSeeOther {
				if wantLoc := "/?msg=" + url.QueryEscape(tc.want); rec.Header().Get("Location") != wantLoc {
					t.Fatalf("location = %q, want %q", rec.Header().Get("Location"), wantLoc)
				}
				if tc.want != "PROVIDER SAVED" {
					t.Fatalf("refusal %q redirected instead of re-rendering the form", tc.want)
				}
				return
			}
			// A refusal re-renders with the typed values still in the form.
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 or 303", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Fatalf("body missing %q", tc.want)
			}
			if tc.want == "PROVIDER SAVED" {
				t.Fatalf("save did not redirect")
			}
			if !strings.Contains(body, `<details class="add-provider" open>`) {
				t.Fatalf("refused save closed the add-provider form")
			}
			for _, v := range []string{tc.form.Get("name"), tc.form.Get("base")} {
				if v == "" {
					continue
				}
				if !strings.Contains(body, `value="`+template.HTMLEscapeString(v)+`"`) {
					t.Fatalf("typed value %q was dropped", v)
				}
			}
		})
	}
}

func TestUIAddProviderPersistsAndRerenders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	for _, base := range []string{"https://api.example.com/v1", "https://api.example.com/v2"} {
		rec := postFormTest(t, h, "/ui/providers", addProviderForm("ACME", base, "ACME_KEY"))
		if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "msg=PROVIDER+SAVED") {
			t.Fatalf("save: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
		}
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, p := range reg {
		if p.Name == "ACME" {
			rows++
			if p.Base != "https://api.example.com/v2" {
				t.Fatalf("re-add must replace the row, base = %q", p.Base)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("ACME rows = %d, want 1", rows)
	}
	home := httptest.NewRecorder()
	h.ServeHTTP(home, dashRequest(http.MethodGet, "/", nil))
	if !strings.Contains(home.Body.String(), "ACME") {
		t.Fatal("home render missing the saved provider")
	}
	sheet := httptest.NewRecorder()
	h.ServeHTTP(sheet, dashRequest(http.MethodGet, "/ui/providers/sheet?name=ACME", nil))
	if sheet.Code != http.StatusOK || !strings.Contains(sheet.Body.String(), "ACME") {
		t.Fatalf("sheet render: status = %d", sheet.Code)
	}
}

// The Msg/MsgMs toast contract: default 6000, clamp 1000..30000.
func TestUIMsgMs(t *testing.T) {
	render := template.Must(template.New("probe").Parse(`{{.Msg}}|{{.MsgMs}}`))
	cases := []struct {
		query string
		msg   string
		ms    int
	}{
		{"", "", 6000},
		{"?msg=Hi&ms=1234", "Hi", 1234},
		{"?msg=Hi", "Hi", 6000},
		{"?ms=999999", "", 30000},
		{"?ms=1", "", 1000},
		{"?ms=abc", "", 6000},
	}
	for _, tc := range cases {
		page := uiHome(httptest.NewRequest(http.MethodGet, "/"+tc.query, nil))
		var out strings.Builder
		if err := render.Execute(&out, page); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("%s|%d", tc.msg, tc.ms); out.String() != want {
			t.Fatalf("%q: rendered %q, want %q", tc.query, out.String(), want)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/providers/sheet?name=Evren+System+One&msg=Hi&ms=1234", nil)
	page, ok := providerSheetPage(req)
	if !ok || page.Msg != "Hi" || page.MsgMs != 1234 {
		t.Fatalf("sheet page = %+v, ok = %v", page, ok)
	}
	combo := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(combo, dashRequest(http.MethodGet, "/ui/combo/sheet?ms=999999", nil))
	if combo.Code != http.StatusOK {
		t.Fatalf("combo sheet status = %d", combo.Code)
	}
}

func TestUIModelFormLoudFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	const name = "Evren System One"
	sheet := "/ui/providers/sheet?name=" + url.QueryEscape(name)
	full := make([]string, maxModelsPerProvider)
	for i := range full {
		full[i] = fmt.Sprintf("m%d", i)
	}
	if err := saveModels(map[string][]string{name: full}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label string
		form  url.Values
		want  string
	}{
		{"missing model on delete", url.Values{"name": {name}, "action": {"remove"}}, "INVALID MODEL"},
		{"empty model on delete", url.Values{"name": {name}, "action": {"remove"}, "model": {""}}, "INVALID MODEL"},
		{"missing action", url.Values{"name": {name}, "model": {"m0"}}, "INVALID ACTION"},
		{"unknown action", url.Values{"name": {name}, "action": {"drop"}, "model": {"m0"}}, "INVALID ACTION"},
		{"remove absent", url.Values{"name": {name}, "action": {"remove"}, "model": {"ghost"}}, "MODEL NOT FOUND"},
		{"duplicate add", url.Values{"name": {name}, "action": {"add"}, "model": {"m0"}}, "MODEL EXISTS"},
		{"models full", url.Values{"name": {name}, "action": {"add"}, "model": {"new"}}, "MODELS FULL"},
	}
	for _, tc := range cases {
		rec := postFormTest(t, h, "/ui/providers/models", tc.form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status = %d", tc.label, rec.Code)
		}
		if want := sheet + "&msg=" + url.QueryEscape(tc.want); rec.Header().Get("Location") != want {
			t.Fatalf("%s: location = %q, want %q", tc.label, rec.Header().Get("Location"), want)
		}
	}
	models, err := loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if list := models[name]; len(list) != maxModelsPerProvider {
		t.Fatalf("failing forms changed the model list: %d entries", len(list))
	}
	rec := postFormTest(t, h, "/ui/providers/models", url.Values{"name": {name}, "action": {"remove"}, "model": {"m0"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid remove status = %d", rec.Code)
	}
	models, err = loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if list := models[name]; len(list) != maxModelsPerProvider-1 {
		t.Fatalf("valid remove left %d entries", len(list))
	}
}

// The add-to-combo form must stay inside the dashboard sheet: its GET renders
// without the home topbar and its POST bounces back into the sheet.
func TestUIComboAddSheet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	const name = "Evren System One"
	if err := saveModels(map[string][]string{name: {"clef"}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, dashRequest(http.MethodGet, "/ui/combo/add/sheet", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sheet status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/ui/combo/add?sheet=1"`) {
		t.Fatalf("sheet form does not post with sheet=1: %q", body)
	}
	if strings.Contains(body, "WALSY") {
		t.Fatalf("sheet rendered the home topbar brand")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, dashRequest(http.MethodGet, "/ui/combo/add", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("standalone add page status = %d", rec.Code)
	}

	rec = postFormTest(t, h, "/ui/combo/add?sheet=1", url.Values{"provider": {name}, "model": {"clef"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/combo/add/sheet?msg=") {
		t.Fatalf("POST location = %q", loc)
	}

	rec = postFormTest(t, h, "/ui/combo/add/sheet", url.Values{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sheet POST status = %d, want 400", rec.Code)
	}
}

// A duplicate key and a full list are refusals, not silent successes; a key for
// a provider that does not exist must not leave a file behind.
func TestUIKeyAddRefusals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	postFormTest(t, h, "/ui/providers", addProviderForm("KeyHost", "https://key.example/v1", "KEYHOST_KEY"))

	add := url.Values{"name": {"KeyHost"}, "action": {"add"}, "key": {"probe-key-1"}}
	if loc := postFormTest(t, h, "/ui/providers/keys", add).Header().Get("Location"); !strings.Contains(loc, "KEY+ADDED") {
		t.Fatalf("first add: location = %q", loc)
	}
	if loc := postFormTest(t, h, "/ui/providers/keys", add).Header().Get("Location"); !strings.Contains(loc, "KEY+ALREADY+SAVED") {
		t.Fatalf("duplicate add: location = %q", loc)
	}
	if keys, _ := loadKeys("KeyHost"); len(keys) != 1 {
		t.Fatalf("duplicate add changed the list: %d keys", len(keys))
	}
	for i := 2; i <= maxKeysPerProvider; i++ {
		add.Set("key", fmt.Sprintf("probe-key-%d", i))
		postFormTest(t, h, "/ui/providers/keys", add)
	}
	add.Set("key", "probe-key-over-limit")
	if loc := postFormTest(t, h, "/ui/providers/keys", add).Header().Get("Location"); !strings.Contains(loc, "KEY+LIMIT+REACHED") {
		t.Fatalf("over-limit add: location = %q", loc)
	}

	ghost := url.Values{"name": {"GhostProvider"}, "action": {"add"}, "key": {"ghost-key-1"}}
	if loc := postFormTest(t, h, "/ui/providers/keys", ghost).Header().Get("Location"); !strings.Contains(loc, "INVALID+PROVIDER") {
		t.Fatalf("ghost provider add: location = %q", loc)
	}
	if keys, _ := loadKeys("GhostProvider"); len(keys) != 0 {
		t.Fatalf("ghost provider left %d keys on disk", len(keys))
	}
}

// RESET restores a baked default; a custom provider has none, so the route must
// refuse instead of deleting the provider and orphaning its stored key.
func TestUIEndpointResetRefusesCustomProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := NewDashboardHandler()
	postFormTest(t, h, "/ui/providers", addProviderForm("Orphan", "https://orphan.example/v1", "ORPHAN_KEY"))
	postFormTest(t, h, "/ui/providers/keys", url.Values{"name": {"Orphan"}, "action": {"add"}, "key": {"orphan-key-1"}})

	loc := postFormTest(t, h, "/ui/providers/reset", url.Values{"name": {"Orphan"}}).Header().Get("Location")
	if !strings.Contains(loc, "NO+DEFAULT+ENDPOINT+TO+RESET") {
		t.Fatalf("custom reset: location = %q", loc)
	}
	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range reg {
		if p.Name == "Orphan" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom provider deleted by reset")
	}
	if keys, _ := loadKeys("Orphan"); len(keys) != 1 {
		t.Fatalf("reset dropped stored keys: %d", len(keys))
	}
}

// Only GET and POST are meaningful on the provider collection; PUT used to fall
// through to the read path and answer 200.
func TestAPIProvidersRejectsOtherMethods(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodPut, "/api/providers", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/providers: status = %d, want 405", rec.Code)
	}
}

// A saved/removed prompt redirects back to the sheet with a msg query; the
// sheet must forward it into the toast and mark it a success, not an error.
func TestUIPromptsSheetSuccessToast(t *testing.T) {
	for _, msg := range []string{"PROMPT SAVED", "PROMPT REMOVED"} {
		t.Run(msg, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			rec := httptest.NewRecorder()
			NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, "/ui/prompts?msg="+url.QueryEscape(msg), nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `<div class="toast toast-ok"`) {
				t.Fatal("prompt sheet missing success toast")
			}
			if strings.Contains(body, "toast-error") {
				t.Fatal("prompt success rendered as an error")
			}
			if !strings.Contains(body, msg) {
				t.Fatalf("prompt sheet missing message %q", msg)
			}
			// PromptSheetMode is excluded from RefreshParent; a write toast here
			// must not ask the home document to reload.
			if strings.Contains(body, `postMessage('walsy-refresh|`) {
				t.Fatal("prompt sheet must not refresh the parent")
			}
		})
	}
}

// Unknown messages keep the error style; adding two write messages must not
// turn every prompt-sheet toast green.
func TestUIPromptsSheetUnknownMsgStaysError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, dashRequest(http.MethodGet, "/ui/prompts?msg=PROMPT+MYSTERY", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<div class="toast toast-error"`) {
		t.Fatal("unknown prompt-sheet message not rendered as an error")
	}
	if strings.Contains(body, "toast-ok") {
		t.Fatal("unknown prompt-sheet message rendered as success")
	}
}
