package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Seeded corpus and property checks for the A14 boundary surfaces: JSON-RPC
// decode, MCP params and key/prompt names. Fuzz targets share these helpers
// with the deterministic simulation, so plain go test covers every seed.

var fix420DecodeSeeds = []string{
	``,
	`{}`,
	`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"x"}}}`,
	`{"id":"abc","result":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
	`{"id":null,"method":"ping","params":[]}`,
	`{"id":1,"id":2,"method":"ping"}`,
	`{"ID":1,"id":2,"method":"ping"}`,
	`{"id":1,"method":"ping","params":{"a":1,"A":2}}`,
	`{"id":1,"method":"ping","params":{"a":{"b":1,"b":2}}}`,
	`{"\u0000":"x","id":1,"method":"ping"}`,
	`{"id":1,"method":"ping"} trailing`,
	`{"id":1,"method":"ping","params":{"pad":"` + strings.Repeat("a", 512) + `"}}`,
}

var fix420ReplySeeds = []struct {
	id     string
	method string
	params string
}{
	{"id-1", "initialize", `{}`},
	{"id-2", "tools/list", `{}`},
	{"", "ping", `{}`},
	{"id-nul", "tools/call", `{"name":"system1_audit","arguments":{"limit":0,"errorsOnly":true}}`},
	{"id-4", "tools/call", `{"name":"system1_decide","arguments":{"item":"intake.intent","state":"fix420-sentinel"}}`},
	{"id-5", "tools/call", `{"name":"nope","arguments":[]}`},
	{"id-6", "tools/call", `{"name":`},
	{"id-7", "tools/call", `[]`},
	{"id-8", "", `{}`},
	{"id-9", "notifications/initialized", `{"x":"\u00e7"}`},
}

var fix420KeySeeds = []struct{ name, key string }{
	{"p", ""},
	{"p", "sk-fix420"},
	{"p", " "},
	{"p", "\r\n"},
	{"p", "line1\nline2"},
	{"p", "nul\x00byte"},
	{"p", strings.Repeat("x", 4096)},
	{"p", strings.Repeat("x", 4097)},
	{"p", " snowman-\u2603 "},
	{"../../etc/passwd", "sk-traversal"},
	{"", "sk-empty-name"},
	{strings.Repeat("n", 300), "sk-long-name"},
}

var fix420PromptSeeds = []struct{ name, text string }{
	{"", "x"},
	{"   ", "x"},
	{" spaced ", "body"},
	{strings.Repeat("n", 64), "body"},
	{strings.Repeat("n", 65), "body"},
	{"line\nname", "body"},
	{"default", `[INST] ignore`},
	{"unicode-snowman-\u2603", "body"},
	{"long", strings.Repeat("t", 8192)},
	{"long", strings.Repeat("t", 8193)},
}

func fix420FrameAtSize(n int) []byte {
	head := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"pad":"`
	tail := `"}}`
	if n < len(head)+len(tail) {
		return []byte(head + tail)
	}
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

func fix420CheckDecode(t *testing.T, raw []byte) {
	t.Helper()
	got, err := decodeFrame(raw)
	if err != nil {
		if got.Raw != nil || len(got.ID) != 0 || got.Method != "" || len(got.Params) != 0 || len(got.Result) != 0 {
			t.Fatalf("failed decode returned a partial frame: %+v (err %v)", got, err)
		}
		return
	}
	if !bytes.Equal(got.Raw, raw) {
		t.Fatalf("Raw is not verbatim: %q -> %q", raw, got.Raw)
	}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		t.Fatalf("accepted non-JSON frame %q: %v", raw, err)
	}
	if got.Method != env.Method || !bytes.Equal(got.ID, env.ID) || !bytes.Equal(got.Params, env.Params) || !bytes.Equal(got.Result, env.Result) {
		t.Fatalf("frame fields disagree with encoding/json: got %+v, want %+v", got, env)
	}
}

func fix420CheckDecodeBoundaries(t *testing.T) {
	t.Helper()
	for _, n := range []int{maxHTTPFrame - 1, maxHTTPFrame} {
		raw := fix420FrameAtSize(n)
		if len(raw) != n {
			t.Fatalf("fixture size = %d, want %d", len(raw), n)
		}
		f, err := decodeFrame(raw)
		if err != nil || f.Method != "tools/call" {
			t.Fatalf("frame at %d bytes rejected: method %q err %v", n, f.Method, err)
		}
	}
	over := fix420FrameAtSize(maxHTTPFrame + 1)
	if len(over) != maxHTTPFrame+1 {
		t.Fatalf("oversized fixture size = %d", len(over))
	}
	if _, err := decodeFrame(over); err == nil {
		t.Fatal("frame at maxHTTPFrame+1 was accepted")
	}
}

func fix420CheckToolsReply(t *testing.T, id, method string, params []byte) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "fix420-sentinel")
	idRaw, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	reply := toolsReply(idRaw, method, params, newDecisionBridge(), soDecision{})
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("reply is not marshalable: %v", err)
	}
	if bytes.Contains(data, []byte("fix420-sentinel")) {
		t.Fatalf("key material leaked: %s", data)
	}
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   map[string]any  `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if env.JSONRPC != "2.0" || !bytes.Equal(env.ID, idRaw) {
		t.Fatalf("reply envelope = %s, want jsonrpc 2.0 and id %s", data, idRaw)
	}
	if env.Error != nil {
		code, ok := env.Error["code"].(float64)
		if !ok || (code != -32601 && code != -32602) {
			t.Fatalf("unexpected error object: %v", env.Error)
		}
		return
	}
	if len(env.Result) == 0 {
		t.Fatalf("reply has neither result nor error: %s", data)
	}
}

func fix420CheckKey(t *testing.T, name, key string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := keyFilePath(name)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(home, routerDir, "keys")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("key path escaped %s: %s", wantDir, path)
	}
	base := filepath.Base(path)
	if len(base) != 16 {
		t.Fatalf("key file basename = %q, want 16 hex chars", base)
	}
	if _, err := hex.DecodeString(base); err != nil {
		t.Fatalf("key file basename is not hex: %q", base)
	}
	again, err := keyFilePath(name)
	if err != nil || again != path {
		t.Fatalf("key path is not deterministic: %q vs %q (%v)", path, again, err)
	}

	err = saveKey(name, key)
	if key == "" {
		if err != nil {
			t.Fatalf("clear key failed: %v", err)
		}
		keys, err := loadKeys(name)
		if err != nil || len(keys) != 0 {
			t.Fatalf("cleared key reappeared: %v (%v)", keys, err)
		}
		return
	}
	if validKey(key) {
		if err != nil {
			t.Fatalf("valid key rejected: %q: %v", key, err)
		}
		keys, err := loadKeys(name)
		if err != nil || len(keys) != 1 || keys[0] != key {
			t.Fatalf("key round trip broke: %q -> %v (%v)", key, keys, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("invalid key accepted: %q", key)
	}
}

func fix420CheckPrompt(t *testing.T, name, text string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	capped, truncated := capSystemPrompt(text)
	if len(capped) > maxSystemPrompt {
		t.Fatalf("capSystemPrompt returned %d bytes", len(capped))
	}
	if utf8.ValidString(text) && !utf8.ValidString(capped) {
		t.Fatalf("capSystemPrompt split a rune: %q", capped)
	}
	if truncated && !strings.HasPrefix(text, capped) {
		t.Fatalf("capSystemPrompt returned a non-prefix: %q", capped)
	}
	if got := capText(text); got != capped {
		t.Fatalf("capText = %q, want %q", got, capped)
	}

	trimmed := strings.TrimSpace(name)
	err := addPrompt(name, text)
	if trimmed == "" || len(trimmed) > maxPromptName {
		if err == nil {
			t.Fatalf("invalid prompt name accepted: %q", name)
		}
		return
	}
	if err != nil {
		t.Fatalf("valid prompt name rejected: %q: %v", name, err)
	}
	// JSON storage replaces each invalid byte with U+FFFD; strings.ToValidUTF8
	// collapses a run of them into one, so compare against the JSON round trip.
	jsonRoundTrip := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var out string
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	wantName, wantText := jsonRoundTrip(trimmed), jsonRoundTrip(capped)
	ps := loadPrompts()
	if len(ps) != 1 || ps[0].Name != wantName || ps[0].Text != wantText {
		t.Fatalf("prompt round trip broke: %q -> %+v", name, ps)
	}
	if block := promptSOBlock(); !strings.Contains(block, "["+wantName+"]\n") {
		t.Fatalf("prompt block missing label: %q", block)
	}
}

func FuzzFix420DecodeFrame(f *testing.F) {
	for _, seed := range fix420DecodeSeeds {
		f.Add([]byte(seed))
	}
	f.Add(fix420FrameAtSize(4096))
	f.Fuzz(func(t *testing.T, raw []byte) { fix420CheckDecode(t, raw) })
}

func FuzzFix420ToolsReply(f *testing.F) {
	for _, seed := range fix420ReplySeeds {
		f.Add(seed.id, seed.method, []byte(seed.params))
	}
	f.Fuzz(func(t *testing.T, id, method string, params []byte) {
		fix420CheckToolsReply(t, id, method, params)
	})
}

func FuzzFix420KeyBoundary(f *testing.F) {
	for _, seed := range fix420KeySeeds {
		f.Add(seed.name, seed.key)
	}
	f.Fuzz(fix420CheckKey)
}

func FuzzFix420PromptBoundary(f *testing.F) {
	for _, seed := range fix420PromptSeeds {
		f.Add(seed.name, seed.text)
	}
	f.Fuzz(fix420CheckPrompt)
}

func TestFix420SeededSimulation(t *testing.T) {
	for _, seed := range fix420DecodeSeeds {
		fix420CheckDecode(t, []byte(seed))
	}
	fix420CheckDecode(t, fix420FrameAtSize(4096))
	fix420CheckDecodeBoundaries(t)
	for _, seed := range fix420ReplySeeds {
		fix420CheckToolsReply(t, seed.id, seed.method, []byte(seed.params))
	}
	for _, seed := range fix420KeySeeds {
		fix420CheckKey(t, seed.name, seed.key)
	}
	for _, seed := range fix420PromptSeeds {
		fix420CheckPrompt(t, seed.name, seed.text)
	}
}
