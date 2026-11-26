package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A single frame that never terminates before the byte cap must stop the tools
// server without producing a reply or allocating without bound.
func TestToolsServerRejectsOversizedFrame(t *testing.T) {
	lr := &frameLimitReader{r: strings.NewReader(strings.Repeat("a", maxToolsFrame+1))}
	if _, err := io.Copy(io.Discard, lr); err != errFrameTooLarge {
		t.Fatalf("frame cap must return errFrameTooLarge, got %v", err)
	}
	frame := `{"id":1,"method":"tools/call","params":"` + strings.Repeat("a", maxToolsFrame+1)
	out := &strings.Builder{}
	runToolsServer(strings.NewReader(frame), out)
	if out.Len() != 0 {
		t.Fatalf("oversized frame produced output: %q", out.String())
	}
}

func toolsSession(t *testing.T, frames ...string) []map[string]any {
	t.Helper()
	out := &strings.Builder{}
	runToolsServer(strings.NewReader(strings.Join(frames, "\n")+"\n"), out)
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad reply %q: %v", line, err)
		}
		replies = append(replies, m)
	}
	return replies
}

func TestToolsServerInitializeListAndCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	replies := toolsSession(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"system1_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(replies) != 4 {
		t.Fatalf("replies = %d, want 4 (notification must be ignored)", len(replies))
	}
	info := replies[0]["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["name"] != "walsy-system1" {
		t.Fatalf("serverInfo = %v", info)
	}
	tools := replies[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %v", tools)
	}
	var decide map[string]any
	for _, raw := range tools {
		def := raw.(map[string]any)
		if def["name"] == "system1_decide" {
			decide = def
		}
	}
	if decide == nil {
		t.Fatalf("system1_decide missing from tools/list: %v", tools)
	}
	schema := decide["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	for _, k := range []string{"item", "state", "phase", "strict"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("inputSchema missing %q: %v", k, props)
		}
	}
	req := schema["required"].([]any)
	if len(req) != 2 || req[0] != "item" || req[1] != "state" {
		t.Fatalf("required = %v", req)
	}
	text := replies[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var status map[string]any
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("status text not JSON: %v", err)
	}
	if _, ok := status["providers"]; !ok {
		t.Fatalf("status missing providers: %s", text)
	}
	if replies[3]["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("unknown tool error = %v", replies[3]["error"])
	}
}

func TestToolsDecideTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	const defaultBody = `{"answers":{"gate":{"type":"choice","choice":"chat","confidence":0.95}}}`
	hits := 0
	status, body := http.StatusOK, defaultBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	cases := []struct {
		name     string
		item     string
		state    string
		status   int
		body     string
		wantHits int
		want     map[string]any
	}{
		{
			name: "local item makes no model call", item: "reasoning.priority", state: "decide which item next",
			want: map[string]any{"item": "reasoning.priority", "admitted": false, "local": true, "steer": "decide locally, no model call"},
		},
		{
			name: "non-proceed label is not admitted", item: "intake.intent", state: "decide intake intent",
			wantHits: 1,
			want: map[string]any{"item": "intake.intent", "label": "chat", "admitted": false, "confidence": 0.95,
				"steer": "hand to main LLM"},
		},
		{
			name: "transport error is an error object", item: "intake.intent", state: "decide intake intent",
			status: http.StatusInternalServerError, body: `{}`, wantHits: 1,
			want: map[string]any{"error": "model unavailable or answer outside the item's label set"},
		},
		{
			name: "low confidence is an error object", item: "intake.intent", state: "decide intake intent",
			body: `{"answers":{"gate":{"type":"choice","choice":"other","confidence":0.5}}}`, wantHits: 1,
			want: map[string]any{"error": "model unavailable or answer outside the item's label set"},
		},
		{
			name: "unknown item is an error object", item: "no.such.item", state: "anything",
			want: map[string]any{"error": "unknown item"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hits, status, body = 0, http.StatusOK, defaultBody
			if c.status != 0 {
				status = c.status
			}
			if c.body != "" {
				body = c.body
			}
			text := toolsDecide(c.item, c.state, "", false)
			var got map[string]any
			if err := json.Unmarshal([]byte(text), &got); err != nil {
				t.Fatalf("reply not JSON: %q: %v", text, err)
			}
			for k, want := range c.want {
				if got[k] != want {
					t.Fatalf("%s = %v, want %v (reply %s)", k, got[k], want, text)
				}
			}
			if _, leaked := got["state"]; leaked {
				t.Fatalf("raw state leaked: %s", text)
			}
			if strings.Contains(text, "test-key") {
				t.Fatalf("key material leaked: %s", text)
			}
			if hits != c.wantHits {
				t.Fatalf("model calls = %d, want %d", hits, c.wantHits)
			}
		})
	}
}

func TestToolsDecideMissingArgs(t *testing.T) {
	cases := []string{
		`{"state":"decide intake intent"}`,
		`{"item":"intake.intent"}`,
		`{}`,
	}
	for _, args := range cases {
		text, err := callTool("system1_decide", json.RawMessage(args), newDecisionBridge())
		if err != nil {
			t.Fatalf("args %s: unexpected transport error: %v", args, err)
		}
		if text != `{"error":"item and state are required"}` {
			t.Fatalf("args %s: reply = %s", args, text)
		}
	}
}

func TestToolsDecideWithoutKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	got := toolsDecide("intake.intent", "decide intake intent", "", false)
	if got != `{"error":"model unavailable or answer outside the item's label set"}` {
		t.Fatalf("no-key reply = %s", got)
	}
	local := toolsDecide("tools.pick", "decide which tool", "", false)
	if !strings.Contains(local, `"local":true`) {
		t.Fatalf("local item needs no key: %s", local)
	}
}

func TestToolsAuditTailAndErrorsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home+"/"+routerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(lines ...string) {
		if err := os.WriteFile(home+"/"+routerDir+"/"+routerAuditName, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(
		`{"event":"relay_frame","status":"ok","mode":"initialize"}`,
		`{"event":"relay_frame","status":"error_-32601","mode":"tools/nope"}`,
		`{"event":"relay_frame","status":"malformed"}`,
	)

	all, err := toolsAudit(2, false)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Count  int               `json:"count"`
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(all), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 2 {
		t.Fatalf("count = %d, want 2", parsed.Count)
	}
	if !strings.Contains(string(parsed.Events[0]), "malformed") {
		t.Fatalf("newest first broken: %s", parsed.Events[0])
	}

	errs, err := toolsAudit(10, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs, "error_-32601") || !strings.Contains(errs, "malformed") || strings.Contains(errs, "initialize") {
		t.Fatalf("errorsOnly = %s", errs)
	}

	if got, err := toolsAudit(10, true); err != nil {
		t.Fatal(err)
	} else if !strings.HasPrefix(got, `{"count":2`) {
		t.Fatalf("errorsOnly count = %s", got)
	}
}

func TestToolsAuditMissingFileIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := toolsAudit(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"count":0,"events":[]}` {
		t.Fatalf("missing audit file = %s", got)
	}
}

func TestToolsAuditRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := home + "/" + routerDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dir + "/audit-target.jsonl"
	if err := os.WriteFile(target, []byte(`{"status":"ok"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir+"/"+routerAuditName); err != nil {
		t.Fatal(err)
	}
	if _, err := toolsAudit(10, false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink audit must fail closed, got err=%v", err)
	}
}

func TestToolsAuditRejectsNonRegular(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := home + "/" + routerDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+"/"+routerAuditName, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := toolsAudit(10, false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory audit must fail closed, got err=%v", err)
	}
}
