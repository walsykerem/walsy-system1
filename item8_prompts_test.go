package main

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

func TestPromptAddListRemove(t *testing.T) {
	item8Home(t)
	if err := addPrompt("alpha", "first text"); err != nil {
		t.Fatal(err)
	}
	if err := addPrompt(" beta ", "second text"); err != nil {
		t.Fatal(err)
	}
	ps := loadPrompts()
	if len(ps) != 2 || ps[0].Name != "alpha" || ps[1].Name != "beta" {
		t.Fatalf("prompts = %+v", ps)
	}
	if err := removePrompt("alpha"); err != nil {
		t.Fatal(err)
	}
	ps = loadPrompts()
	if len(ps) != 1 || ps[0].Name != "beta" || ps[0].Text != "second text" {
		t.Fatalf("after remove = %+v", ps)
	}
	if err := addPrompt("", "no name"); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := addPrompt(strings.Repeat("n", maxPromptName+1), "long name"); err == nil {
		t.Fatal("oversized name accepted")
	}
}

func TestPromptCap8193(t *testing.T) {
	item8Home(t)
	if err := addPrompt("big", strings.Repeat("x", 8193)); err != nil {
		t.Fatal(err)
	}
	ps := loadPrompts()
	if len(ps) != 1 || len(ps[0].Text) != 8192 {
		t.Fatalf("capped text len = %d", len(ps[0].Text))
	}
	capped, truncated := capSystemPrompt(strings.Repeat("x", 8193))
	if len(capped) != 8192 || !truncated {
		t.Fatalf("cap = %d, truncated=%v", len(capped), truncated)
	}
}

func TestPromptFileMode(t *testing.T) {
	home := item8Home(t)
	if err := addPrompt("alpha", "text"); err != nil {
		t.Fatal(err)
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

	state := buildSOState(`{"state":"classify this request","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`)
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

	ps := loadPrompts()
	if len(ps) != 1 || ps[0].Name != "default" || !strings.Contains(ps[0].Text, "CHECKMULTI7A3F") {
		t.Fatalf("migration = %+v", ps)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file removed: %v", err)
	}
	if again := loadPrompts(); len(again) != 1 {
		t.Fatalf("second load duplicated: %+v", again)
	}
}

func TestPromptNoPromptsNoBlock(t *testing.T) {
	item8Home(t)
	if block := promptSOBlock(); block != "" {
		t.Fatalf("empty block = %q", block)
	}
}
