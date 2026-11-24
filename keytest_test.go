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

// keyTestResult names the answer the endpoint gave. Every status maps to its
// own name: a refused request must not be reported as a forbidden key.
func TestKeyTestResultNamesTheStatus(t *testing.T) {
	cases := map[int]string{
		0:                              "ENDPOINT UNREACHABLE",
		http.StatusOK:                  "ENDPOINT RESPONDED",
		http.StatusBadRequest:          "BAD REQUEST",
		http.StatusUnauthorized:        "INVALID KEY",
		http.StatusForbidden:           "FORBIDDEN",
		http.StatusNotFound:            "MODEL NOT FOUND",
		http.StatusTooManyRequests:     "RATE LIMITED",
		http.StatusUnprocessableEntity: "BAD REQUEST",
		http.StatusInternalServerError: "SERVER ERROR",
	}
	for status, want := range cases {
		if got := keyTestResult(status); got != want {
			t.Fatalf("keyTestResult(%d) = %q, want %q", status, got, want)
		}
	}
}

// A 400 from the endpoint is a refused request, not a forbidden credential.
// The old answer was a flat FORBIDDEN for every non-200 answer, which sent the
// user looking for a key problem that did not exist.
func TestKeyTestReportsBadRequestInsteadOfForbidden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	t.Setenv("HOME", t.TempDir())
	check(t, saveCustomProvider("TEST-P", upstream.URL, "TEST_KEY") == nil, "seed provider")
	check(t, saveModels(map[string][]string{"TEST-P": {"m1"}}) == nil, "seed models")
	check(t, addKey("TEST-P", "sk-test-secret-do-not-render") == nil, "seed key")

	s := httptest.NewServer(NewDashboardHandler())
	defer s.Close()
	resp, err := http.Post(s.URL+"/api/keytest", "application/json", strings.NewReader(`{"provider":"TEST-P","model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Status int    `json:"status"`
		Result string `json:"result"`
		Winner string `json:"winner"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if out.Status != http.StatusBadRequest || out.Result != "BAD REQUEST" || out.Winner != "" {
		t.Fatalf("400 must be reported as a refused request: %+v", out)
	}
}

func TestTestProviderKeyReportsCoarseResult(t *testing.T) {
	const secret = "sk-test-secret-do-not-render"
	var gotMethod, gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	check(t, addKey("TEST-LOCAL", secret) == nil, "seed key")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".walsy-system1")
	check(t, os.MkdirAll(dir, 0700) == nil, "state dir")
	writeProviders(t, dir, "TEST-LOCAL, "+srv.URL+", TEST_KEY\n", 0600)

	got := testProviderKey("TEST-LOCAL", 1)
	check(t, got == "KEY OK", "2xx probe -> KEY OK, got "+got)
	check(t, gotMethod == http.MethodPost, "probe must POST, got "+gotMethod)
	check(t, gotAuth == "Bearer "+secret, "bearer header sent")
	check(t, gotAPIKey == secret, "x-api-key header sent")
	check(t, !strings.Contains(got, secret), "result must not echo key")
}
