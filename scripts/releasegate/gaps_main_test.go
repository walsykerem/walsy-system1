package main

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// gapMainStdout runs fn with os.Stdout captured so the pass line is asserted.
func gapMainStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = old
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// main returns on the PASS path, so it runs in-process against a loopback
// stub: repo release.tag/CHANGELOG are read from the real tree, everything
// external is env-driven.
func TestGapScriptMain(t *testing.T) {
	t.Chdir("../..")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answers":{"final":{"choice":"complete","confidence":0.9}}}`)
	}))
	defer srv.Close()
	t.Setenv("WALSY_SO_ENDPOINT", srv.URL)
	t.Setenv("EVREN_API_KEY", "gap-dummy-key")
	t.Setenv("RELEASE_APPROVAL", "gap-test")

	oldArgs, oldFlags := os.Args, flag.CommandLine
	defer func() { os.Args, flag.CommandLine = oldArgs, oldFlags }()
	flag.CommandLine = flag.NewFlagSet("releasegate", flag.ContinueOnError)
	os.Args = []string{"releasegate", "-so-final-complete=0.8"}
	if err := run(0.8); err != nil {
		t.Fatalf("run before main: %v", err)
	}
	if out := gapMainStdout(t, main); !strings.Contains(out, "release gate: PASS") {
		t.Fatalf("main output = %q, want PASS", out)
	}
}
