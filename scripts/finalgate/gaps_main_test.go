package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type gapMainRoundTrip func(*http.Request) (*http.Response, error)

func (f gapMainRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// gapMainStdout runs fn with os.Stdout captured so the verdict text is asserted.
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

// main returns on the all-done path, so it runs in-process: every claim must
// resolve (proved first, so a drift can never fall through to os.Exit), and the
// endpoint is stubbed at the transport so no network is touched.
func TestGapScriptMain(t *testing.T) {
	t.Chdir("../..")
	t.Setenv("EVREN_API_KEY", "gap-dummy-key")
	for _, c := range claims {
		if err := resolve(c); err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
	}
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	http.DefaultTransport = gapMainRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"answers":{"claim":{"choice":"done","confidence":0.95}}}`)),
		}, nil
	})
	out := gapMainStdout(t, main)
	for _, want := range []string{
		"AGGREGATE",
		fmt.Sprintf("VERDICT COMPLETE %d/%d", len(claims), len(claims)),
		"open 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("finalgate output omits %q:\n%s", want, out)
		}
	}
}
