package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The scripted SO answers drive every harness case; their default and queue
// consumption had no test.
func TestGapScriptNextDefaultsAndQueue(t *testing.T) {
	defer script()
	script()
	def := answer{Choice: "yes", Conf: 0.95}
	if got := next(); got != def {
		t.Fatalf("empty queue = %+v, want %+v", got, def)
	}
	script(answer{"no", 0.9}, answer{"other", 0.8})
	for _, want := range []answer{{"no", 0.9}, {"other", 0.8}, def, def} {
		if got := next(); got != want {
			t.Fatalf("next = %+v, want %+v", got, want)
		}
	}
}

// withCaps supplies the enforcing bridge's opt-in fields; dropping one makes
// every harness frame fail for the wrong reason.
func TestGapScriptWithCapsAddsRequiredFields(t *testing.T) {
	caps := []string{"need_label", "no_scratch", "safe", "reversible", "authority", "calibrated", "opt_in"}
	cases := []struct {
		name   string
		params json.RawMessage
		keep   map[string]any
	}{
		{"empty", nil, nil},
		{"existing fields", json.RawMessage(`{"name":"read_file","arguments":{"path":"/x"},"phase":"custom"}`), map[string]any{
			"name":      "read_file",
			"arguments": map[string]any{"path": "/x"},
		}},
		{"malformed params", json.RawMessage(`[1,2]`), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := withCaps(frame{ID: 9, Method: "tools/call", Params: tc.params})
			if f.ID != 9 || f.Method != "tools/call" {
				t.Fatalf("frame = %+v", f)
			}
			var p map[string]any
			if err := json.Unmarshal(f.Params, &p); err != nil {
				t.Fatal(err)
			}
			for _, k := range caps {
				if p[k] != true {
					t.Fatalf("cap %q = %v, want true", k, p[k])
				}
			}
			if p["phase"] != "before-model" {
				t.Fatalf("phase = %v, want before-model", p["phase"])
			}
			for k, v := range tc.keep {
				if !reflect.DeepEqual(p[k], v) {
					t.Fatalf("field %q = %v, want %v", k, p[k], v)
				}
			}
		})
	}
}

// checkDisk measures refusals on disk; check requires a real assertion. Both
// had only their empty/wrong-input branches covered.
func TestGapScriptCheckDiskAndCheck(t *testing.T) {
	fail = 0
	defer func() { fail = 0 }()
	dir := t.TempDir()
	missing := filepath.Join(dir, "never.txt")
	checkDisk("absent", missing, "SECRET")
	if fail != 0 {
		t.Fatalf("absent file: fail = %d, want 0", fail)
	}
	present := filepath.Join(dir, "present.txt")
	if err := os.WriteFile(present, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkDisk("clean", present, "SECRET")
	if fail != 0 {
		t.Fatalf("clean file: fail = %d, want 0", fail)
	}
	if err := os.WriteFile(present, []byte("has SECRET here"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkDisk("secret", present, "SECRET")
	if fail != 1 {
		t.Fatalf("forbidden content: fail = %d, want 1", fail)
	}
	fail = 0
	checkDisk("directory", dir, "SECRET")
	if fail != 1 {
		t.Fatalf("unreadable path: fail = %d, want 1", fail)
	}
	fail = 0
	check("all wanted", "alpha beta", "alpha", "beta")
	if fail != 0 {
		t.Fatalf("all wanted: fail = %d, want 0", fail)
	}
	check("one missing", "alpha", "alpha", "beta")
	if fail != 1 {
		t.Fatalf("one missing: fail = %d, want 1", fail)
	}
}

// msg is how the transcript prints a blocked call; its error shape was only
// consumed indirectly.
func TestGapScriptMsgAndCall(t *testing.T) {
	if got := msg(nil); got != "" {
		t.Fatalf("msg(nil) = %q", got)
	}
	if got := msg(map[string]any{"error": map[string]any{"message": "blocked: no"}}); got != "blocked: no" {
		t.Fatalf("msg(error) = %q", got)
	}
	if got := msg(map[string]any{"error": map[string]any{}}); got != "" {
		t.Fatalf("msg(error without message) = %q", got)
	}
	if got := msg(map[string]any{"result": map[string]any{"path": "/x"}}); !strings.Contains(got, "/x") {
		t.Fatalf("msg(result) = %q", got)
	}
	f := call(7, "write_file", map[string]any{"path": "/x"})
	if f.ID != 7 || f.Method != "tools/call" {
		t.Fatalf("call frame = %+v", f)
	}
	var p map[string]any
	if err := json.Unmarshal(f.Params, &p); err != nil {
		t.Fatal(err)
	}
	if p["name"] != "write_file" {
		t.Fatalf("tool name = %v", p["name"])
	}
	args, ok := p["arguments"].(map[string]any)
	if !ok || args["path"] != "/x" {
		t.Fatalf("arguments = %v", p["arguments"])
	}
}

// shaFile prints the provenance hashes; a wrong digest would make the
// transcript's source claim false.
func TestGapScriptShaFileKnownDigest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "abc.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := shaFile(p); got != want {
		t.Fatalf("shaFile = %q, want %q", got, want)
	}
}

// The fake SO must emit exactly the typed choice the shipped gate parses.
func TestGapScriptEndpointAnswersGateShape(t *testing.T) {
	script(answer{Choice: "other", Conf: 0.77})
	defer script()
	ep := endpoint()
	cl := &http.Client{Timeout: 2 * time.Second}
	for i, want := range []answer{{"other", 0.77}, {"yes", 0.95}} {
		resp, err := cl.Post(ep, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Answers map[string]struct {
				Type       string  `json:"type"`
				Choice     string  `json:"choice"`
				Confidence float64 `json:"confidence"`
			} `json:"answers"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		g, ok := out.Answers["gate"]
		if !ok || g.Type != "choice" || g.Choice != want.Choice || g.Confidence != want.Conf {
			t.Fatalf("reply %d = %+v, want %+v", i, g, want)
		}
	}
}
