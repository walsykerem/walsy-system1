package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
