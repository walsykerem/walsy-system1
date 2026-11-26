package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func audEAuditReply(t *testing.T, args string) string {
	t.Helper()
	replies := toolsSession(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_audit","arguments":`+args+`}}`)
	if len(replies) != 1 {
		t.Fatalf("audit replies = %d, want 1: %v", len(replies), replies)
	}
	if errObj, ok := replies[0]["error"]; ok {
		t.Fatalf("audit error: %v", errObj)
	}
	return replies[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
}

func audEAuditError(t *testing.T, args string) map[string]any {
	t.Helper()
	replies := toolsSession(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system1_audit","arguments":`+args+`}}`)
	if len(replies) != 1 {
		t.Fatalf("audit replies = %d, want 1: %v", len(replies), replies)
	}
	errObj, ok := replies[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("audit did not fail closed: %v", replies[0])
	}
	return errObj
}

type audEAuditPayload struct {
	Count  int               `json:"count"`
	Events []json.RawMessage `json:"events"`
}

// The live audit file can pass the rotate-by-size check and still grow past
// maxToolsFrame; the newest error must remain the first event returned.
func TestAuditTailNewestErrorAfterLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, routerAuditName)
	ok := `{"event":"relay_frame","status":"ok","mode":"initialize"}` + "\n"
	errLine := `{"event":"relay_frame","status":"error_-32001","code":-32001,"mode":"tools/call"}` + "\n"
	raw := strings.Repeat(ok, maxToolsFrame/len(ok)+1) + errLine
	if len(raw) <= maxToolsFrame {
		t.Fatalf("fixture %d bytes must exceed %d", len(raw), maxToolsFrame)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	var parsed audEAuditPayload
	if err := json.Unmarshal([]byte(audEAuditReply(t, `{"limit":10,"errorsOnly":true}`)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 1 {
		t.Fatalf("count = %d, want 1: %+v", parsed.Count, parsed)
	}
	if !strings.Contains(string(parsed.Events[0]), `"status":"error_-32001"`) || !strings.Contains(string(parsed.Events[0]), `"code":-32001`) {
		t.Fatalf("newest error missing/first broken: %s", parsed.Events[0])
	}

	// Small files still scan newest-first from the start of the file.
	if err := os.WriteFile(path, []byte(ok+errLine), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed = audEAuditPayload{}
	if err := json.Unmarshal([]byte(audEAuditReply(t, `{"limit":2}`)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 2 || !strings.Contains(string(parsed.Events[0]), `"code":-32001`) || !strings.Contains(string(parsed.Events[1]), `"status":"ok"`) {
		t.Fatalf("small-file newest-first broken: %+v", parsed)
	}
}

// The audit tool behind the JSON-RPC entry must treat a missing file as empty
// and refuse a path that is not a regular file.
func TestAuditToolFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, routerAuditName)

	if got := audEAuditReply(t, `{}`); got != `{"count":0,"events":[]}` {
		t.Fatalf("missing audit file = %s", got)
	}

	target := filepath.Join(home, "real.jsonl")
	if err := os.WriteFile(target, []byte(`{"status":"error_-32001"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	errObj := audEAuditError(t, `{}`)
	if errObj["code"] != float64(-32602) || !strings.Contains(errObj["message"].(string), "not a regular file") {
		t.Fatalf("symlink error = %v", errObj)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	errObj = audEAuditError(t, `{}`)
	if errObj["code"] != float64(-32602) || !strings.Contains(errObj["message"].(string), "not a regular file") {
		t.Fatalf("directory error = %v", errObj)
	}
}
