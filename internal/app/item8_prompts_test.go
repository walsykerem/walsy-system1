package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// item8Home isolates the whole ~/.walsy-system1 tree for one test.
func item8Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestPromptFileMode(t *testing.T) {
	home := item8Home(t)
	srv, _ := dashboardTestServer(t)
	if code := postJSONTest(t, srv.URL+"/api/prompts/add", map[string]string{"name": "alpha", "text": "text"}, nil); code != http.StatusOK {
		t.Fatalf("prompt add status = %d", code)
	}
	path := filepath.Join(home, ".walsy-system1", promptsFile)
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("prompts.json mode: %v, %v", st, err)
	}
}

// TestPromptSOBlockMultiMulti7A3F checks named prompts stay in the local UI
// block and never reach SO in the state string, through a real askSOQ POST
// against an httptest server.
func TestPromptSOBlockMultiMulti7A3F(t *testing.T) {
	item8Home(t)
	const token = "CHECKMULTI7A3F"
	if err := addPrompt("one", token+" keep this line"); err != nil {
		t.Fatal(err)
	}
	if err := addPrompt("two", "second named prompt"); err != nil {
		t.Fatal(err)
	}

	block := promptSOBlock()
	for _, want := range []string{"## USER PROMPTS", "[one]", "[two]", token} {
		if !strings.Contains(block, want) {
			t.Fatalf("prompt block missing %q: %s", want, block)
		}
	}

	state := buildSOState(`{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`)
	var e stateEnvelope
	if err := json.Unmarshal([]byte(state), &e); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.UserSystemPrompts, "[one]") || strings.Contains(e.UserSystemPrompts, "[two]") || strings.Contains(e.UserSystemPrompts, token) {
		t.Fatalf("SO envelope leaked named prompts: %+v", e)
	}

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			got = req.State
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.9}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()
	t.Setenv("EVREN_API_KEY", "test-key")
	_ = route(state, nil)
	if got == "" {
		t.Fatal("route did not post SO state")
	}
	for _, leak := range []string{"## USER PROMPTS", "[one]", "[two]", token} {
		if strings.Contains(got, leak) {
			t.Fatalf("SO state leaked %q: %s", leak, got)
		}
	}
}

func TestPromptLegacyMigration(t *testing.T) {
	home := item8Home(t)
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, systemPromptFile)
	if err := os.WriteFile(legacy, []byte("legacy token CHECKMULTI7A3F"), 0600); err != nil {
		t.Fatal(err)
	}

	srv, _ := dashboardTestServer(t)
	fetch := func() []Prompt {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/prompts")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ps []Prompt
		if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
			t.Fatal(err)
		}
		return ps
	}

	ps := fetch()
	if len(ps) != 1 || ps[0].Name != "default" || !strings.Contains(ps[0].Text, "CHECKMULTI7A3F") {
		t.Fatalf("migration = %+v", ps)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file removed: %v", err)
	}
	if again := fetch(); len(again) != 1 {
		t.Fatalf("second load duplicated: %+v", again)
	}
}
