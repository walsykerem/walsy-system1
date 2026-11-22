package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

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
	if len(tools) != 2 {
		t.Fatalf("tools = %v", tools)
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
