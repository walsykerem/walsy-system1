package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type gapRoundTrip func(*http.Request) (*http.Response, error)

func (f gapRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ask's wire format and failure branches were untested: the endpoint is baked
// into the command, so intercept DefaultTransport with a loopback-free stub.
func TestGapScriptAskParsesVerdictAndHeaders(t *testing.T) {
	t.Setenv("EVREN_API_KEY", "gap-dummy-key")
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()

	type captured struct {
		method, url, key, ctype string
		body                    map[string]any
	}
	cases := []struct {
		name, reply, wantErr, want string
		status                     int
		conf                       float64
	}{
		{"done", `{"answers":{"claim":{"choice":"done","confidence":0.93}}}`, "", "done", 200, 0.93},
		{"missing label is returned", `{"answers":{"claim":{"choice":"missing","confidence":0.4}}}`, "", "missing", 200, 0.4},
		{"http error", `boom`, "http 503", "", 503, 0},
		{"malformed json", `{not json`, "invalid character", "", 200, 0},
		{"no answer", `{"answers":{}}`, "no label", "", 200, 0},
		{"empty choice", `{"answers":{"claim":{"choice":"","confidence":0.9}}}`, "no label", "", 200, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got captured
			http.DefaultTransport = gapRoundTrip(func(r *http.Request) (*http.Response, error) {
				got.method = r.Method
				got.url = r.URL.String()
				got.key = r.Header.Get("x-api-key")
				got.ctype = r.Header.Get("Content-Type")
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &got.body)
				return &http.Response{
					StatusCode: tc.status,
					Body:       io.NopCloser(strings.NewReader(tc.reply)),
					Header:     make(http.Header),
				}, nil
			})
			choice, conf, err := ask(claim{"KX", "claim text"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ask error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if choice != tc.want || conf != tc.conf {
				t.Fatalf("ask = (%q, %v), want (%q, %v)", choice, conf, tc.want, tc.conf)
			}
			if got.method != http.MethodPost || got.url != "https://evren-llmapi.ssyz.org.tr/v1/systemone" {
				t.Fatalf("request = %s %s", got.method, got.url)
			}
			if got.key != "gap-dummy-key" || !strings.Contains(got.ctype, "application/json") {
				t.Fatalf("headers = key %q ctype %q", got.key, got.ctype)
			}
			if got.body["model"] != "clef" || got.body["state"] != "KX: claim text" {
				t.Fatalf("body = %v", got.body)
			}
			q, ok := got.body["questions"].(map[string]any)
			if !ok {
				t.Fatalf("questions = %v", got.body["questions"])
			}
			cq, ok := q["claim"].(map[string]any)
			if !ok || cq["type"] != "choice" {
				t.Fatalf("claim question = %v", q["claim"])
			}
			crit, ok := cq["criteria"].(map[string]any)
			if !ok {
				t.Fatalf("criteria = %v", cq["criteria"])
			}
			for _, label := range []string{"done", "missing"} {
				if _, ok := crit[label]; !ok {
					t.Fatalf("criteria missing %q: %v", label, crit)
				}
			}
		})
	}
}

// key() has two sources; neither was pinned, and a stray real key would make
// the ask test depend on the developer's HOME.
func TestGapScriptKeyEnvAndFileFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EVREN_API_KEY", "")
	if got := key(); got != "" {
		t.Fatalf("key with no env and no file = %q, want empty", got)
	}
	dir := filepath.Join(home, ".walsy-system1", "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "f0c1452bcf623bb1")
	if err := os.WriteFile(path, []byte("file-key\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := key(); got != "file-key" {
		t.Fatalf("key from file = %q, want file-key", got)
	}
	t.Setenv("EVREN_API_KEY", "env-key")
	if got := key(); got != "env-key" {
		t.Fatalf("key from env = %q, want env-key", got)
	}
}

// splitCite's ignored Atoi errors and range shape had no direct coverage.
func TestGapScriptSplitCite(t *testing.T) {
	cases := []struct {
		tok      string
		path     string
		from, to int
	}{
		{"a.go", "a.go", 0, 0},
		{"dir/a.go:7", "dir/a.go", 7, 0},
		{"dir/a.go:7-9", "dir/a.go", 7, 9},
		{"a.go:x", "a.go", 0, 0},
		{"a.go:7-x", "a.go", 7, 0},
		{"a.go:2-1", "a.go", 2, 1},
	}
	for _, tc := range cases {
		path, from, to := splitCite(tc.tok)
		if path != tc.path || from != tc.from || to != tc.to {
			t.Errorf("splitCite(%q) = (%q, %d, %d), want (%q, %d, %d)", tc.tok, path, from, to, tc.path, tc.from, tc.to)
		}
	}
}

// citations must skip runtime paths, dedupe, and only count a bare filename
// when that file exists.
func TestGapScriptCitationsFiltersAndDedupes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "present.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got := citations("see present.go:3-7, again present.go:3-7, /tmp/abs.go:2, absent.go, present.go")
	want := []citation{{path: "present.go", from: 3, to: 7}, {path: "present.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("citations = %v, want %v", got, want)
	}
}

// excerpt's window clamp and 40-line inline cap were untested.
func TestGapScriptExcerptWindowAndLimit(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%03d", i+1)
	}
	if err := os.WriteFile(filepath.Join(dir, "lines.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	cases := []struct {
		name, header, present, absent string
		from, to                      int
	}{
		{"leading clamp", "--- lines.txt:1-4 ---", "L001", "L005", 0, 0},
		{"middle window", "--- lines.txt:47-53 ---", "L050", "L046", 50, 50},
		{"wide range capped", "--- lines.txt:1-40 ---", "L040", "L041", 1, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex, err := excerpt(citation{"lines.txt", tc.from, tc.to})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(ex, tc.header+"\n") {
				t.Fatalf("header = %q, want %q", ex, tc.header)
			}
			if !strings.Contains(ex, tc.present) || strings.Contains(ex, tc.absent) {
				t.Fatalf("window wrong:\n%s", ex)
			}
		})
	}
	ex, err := excerpt(citation{"one.txt", 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ex, "--- one.txt:1 ---\n") || !strings.Contains(ex, "only") {
		t.Fatalf("single line excerpt = %q", ex)
	}
	if _, err := excerpt(citation{"lines.txt", 2, 101}); err == nil {
		t.Fatal("out-of-range citation did not fail")
	}
}
