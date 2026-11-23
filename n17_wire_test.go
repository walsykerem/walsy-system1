package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestN17WireSkeletonAndCap covers the live SO state path: only prompt ids and
// SHA-256 digests reach a POST; an over-cap state is rejected before any POST.
func TestN17WireSkeletonAndCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const token = "CHECK-N17-WIRE-DO-NOT-SEND"
	if err := addPrompt("one", token+" keep this body local"); err != nil {
		t.Fatal(err)
	}

	var posts int
	var wire string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		b, _ := io.ReadAll(r.Body)
		wire = string(b)
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.9}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()
	t.Setenv("EVREN_API_KEY", "test-key")

	state := buildSOState(`{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`)
	if state == "" {
		t.Fatal("valid state rejected by skeleton build")
	}
	if strings.Contains(state, token) || strings.Contains(state, "keep this body local") {
		t.Fatalf("buildSOState leaked prompt body: %s", state)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(state), &obj); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"user_system_prompt", "user_system_prompts", "body", "text"} {
		if _, ok := obj[forbidden]; ok {
			t.Fatalf("SO state has forbidden field %q: %s", forbidden, state)
		}
	}
	prompts, ok := obj["prompts"].([]any)
	if !ok || len(prompts) != 1 {
		t.Fatalf("SO prompt refs = %v", obj["prompts"])
	}
	ref, ok := prompts[0].(map[string]any)
	if !ok || ref["id"] != "one" {
		t.Fatalf("SO prompt ref = %v", prompts[0])
	}
	sum := sha256.Sum256([]byte(token + " keep this body local"))
	if ref["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("SO prompt digest = %v", ref["sha256"])
	}
	if _, ok := ref["body"]; ok {
		t.Fatalf("SO prompt ref has body: %v", ref)
	}

	d := askSOQOnce(state, soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}})
	if d.Choice != "no" {
		t.Fatalf("SO answer = %+v", d)
	}
	if posts != 1 {
		t.Fatalf("SO posts = %d, want 1", posts)
	}
	if strings.Contains(wire, token) || strings.Contains(wire, "keep this body local") {
		t.Fatalf("SO POST leaked prompt body: %s", wire)
	}

	// An over-cap state is rejected, not truncated; route() cannot POST it.
	posts = 0
	wire = ""
	huge := strings.Repeat("x", maxSOStateChars+1)
	over := buildSOState(`{"state":"` + huge + `","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`)
	if over != "" {
		t.Fatalf("over-cap state built: %d chars", len(over))
	}
	r := route(over, nil)
	if r.Decision != decPass || r.Reason != "no-state" {
		t.Fatalf("over-cap route = %+v", r)
	}
	if posts != 0 || wire != "" {
		t.Fatalf("over-cap state reached POST: posts=%d body=%q", posts, wire)
	}
}
