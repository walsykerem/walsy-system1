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

// decodeResponse rejects unknown fields; only the happy path was covered.
func TestGapScriptDecodeResponseUnknownFields(t *testing.T) {
	cases := []struct {
		name, raw, wantErr string
	}{
		{"unknown top field", `{"model":"clef","surprise":1}`, "surprise"},
		{"unknown nested field", `{"answers":{"final":{"choice":"complete","surprise":1}}}`, "surprise"},
		{"truncated json", `{"answers":{"final":`, "EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeResponse(strings.NewReader(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("decodeResponse error = %v, want %q", err, tc.wantErr)
			}
		})
	}
	out, err := decodeResponse(strings.NewReader(`{}`))
	if err != nil || len(out.Answers) != 0 {
		t.Fatalf("empty object = %+v, %v", out, err)
	}
}

// findRoot picks the nearest release.tag and fails closed when none exists.
func TestGapScriptFindRootNearestTag(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte("0.1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	got, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil || gotEval != want {
		t.Fatalf("findRoot = %q (%q), want %q: %v", got, gotEval, want, err)
	}
	nested := filepath.Join(root, "a")
	if err := os.WriteFile(filepath.Join(nested, "release.tag"), []byte("2.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = findRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, err = filepath.EvalSymlinks(nested)
	if err != nil {
		t.Fatal(err)
	}
	gotEval, err = filepath.EvalSymlinks(got)
	if err != nil || gotEval != want {
		t.Fatalf("nearest findRoot = %q (%q), want %q: %v", got, gotEval, want, err)
	}
	t.Chdir(t.TempDir())
	if _, err := findRoot(); err == nil {
		t.Fatal("findRoot with no release.tag did not fail")
	}
}

// run() had no test at all: it must build the documented request from a local
// throwaway tree and accept a loopback verdict.
func gapReleaseSetup(t *testing.T, version, changelog string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "release.tag"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte(changelog), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())
}

func TestGapScriptRunBuildsRequestAndAcceptsVerdict(t *testing.T) {
	gapReleaseSetup(t, "9.9.9", "# 9.9.9\nlocal checks\n")
	t.Setenv("RELEASE_APPROVAL", "gap-approver")
	t.Setenv("EVREN_API_KEY", "gap-dummy-key")
	var got struct {
		method, key, ctype string
		body               map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.key = r.Header.Get("x-api-key")
		got.ctype = r.Header.Get("content-type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"clef","routed_model":"Cloudflare/clef","answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`)
	}))
	defer srv.Close()
	t.Setenv("WALSY_SO_ENDPOINT", srv.URL)

	if err := run(0.8); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.method != http.MethodPost || got.key != "gap-dummy-key" || !strings.Contains(got.ctype, "application/json") {
		t.Fatalf("request = %s, key %q, ctype %q", got.method, got.key, got.ctype)
	}
	if got.body["model"] != "clef" {
		t.Fatalf("model = %v", got.body["model"])
	}
	state, _ := got.body["state"].(string)
	for _, want := range []string{"9.9.9", "NOT a release decision", "PENDING"} {
		if !strings.Contains(state, want) {
			t.Fatalf("state %q omits %q", state, want)
		}
	}
	q, ok := got.body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions = %v", got.body["questions"])
	}
	final, ok := q["final"].(map[string]any)
	if !ok || final["type"] != "choice" {
		t.Fatalf("final question = %v", q["final"])
	}
	crit, ok := final["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("criteria = %v", final["criteria"])
	}
	for _, label := range []string{"complete", "incomplete", "hold"} {
		if _, ok := crit[label]; !ok {
			t.Fatalf("criteria missing %q: %v", label, crit)
		}
	}
}

// Every precondition and rejection in run() must fail before a verdict is
// accepted, including the exact confidence boundary.
func TestGapScriptRunRejections(t *testing.T) {
	good := `{"answers":{"final":{"type":"choice","choice":"complete","confidence":0.9}}}`
	cases := []struct {
		name, changelog, approval, key, reply string
		status                                int
		thr                                   float64
		wantErr                               string
	}{
		{"tag absent from changelog", "# 2.0.0", "a", "k", good, 200, 0.8, "absent from CHANGELOG"},
		{"no approver", "0.1.0", "", "k", good, 200, 0.8, "no RELEASE_APPROVAL"},
		{"no key", "0.1.0", "a", "", good, 200, 0.8, "no System One key"},
		{"http error", "0.1.0", "a", "k", "", 503, 0.8, "System One status 503"},
		{"no final answer", "0.1.0", "a", "k", `{"answers":{}}`, 200, 0.8, "no final answer"},
		{"bad label", "0.1.0", "a", "k", `{"answers":{"final":{"choice":"hold","confidence":0.99}}}`, 200, 0.8, "not complete"},
		{"low confidence", "0.1.0", "a", "k", `{"answers":{"final":{"choice":"complete","confidence":0.5}}}`, 200, 0.8, "not complete"},
		{"boundary passes", "0.1.0", "a", "k", `{"answers":{"final":{"choice":"complete","confidence":0.8}}}`, 200, 0.8, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gapReleaseSetup(t, "0.1.0", tc.changelog)
			t.Setenv("RELEASE_APPROVAL", tc.approval)
			t.Setenv("EVREN_API_KEY", tc.key)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer srv.Close()
			t.Setenv("WALSY_SO_ENDPOINT", srv.URL)
			err := run(tc.thr)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("run error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
