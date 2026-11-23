package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The live audit file can pass the rotate-by-size check and still grow past
// maxToolsFrame; the newest error must remain the first event returned.
func TestAuditTailNewestErrorAfterLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home+"/"+routerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := home + "/" + routerDir + "/" + routerAuditName
	ok := `{"event":"relay_frame","status":"ok","mode":"initialize"}` + "\n"
	errLine := `{"event":"relay_frame","status":"error_-32001","code":-32001,"mode":"tools/call"}` + "\n"
	raw := strings.Repeat(ok, maxToolsFrame/len(ok)+1) + errLine
	if len(raw) <= maxToolsFrame {
		t.Fatalf("fixture %d bytes must exceed %d", len(raw), maxToolsFrame)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := toolsAudit(10, true)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Count  int               `json:"count"`
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 1 {
		t.Fatalf("count = %d, want 1: %s", parsed.Count, got)
	}
	if !strings.Contains(string(parsed.Events[0]), `"status":"error_-32001"`) || !strings.Contains(string(parsed.Events[0]), `"code":-32001`) {
		t.Fatalf("newest error missing/first broken: %s", parsed.Events[0])
	}

	// Small files still scan newest-first from the start of the file.
	if err := os.WriteFile(path, []byte(ok+errLine), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = toolsAudit(2, false)
	if err != nil {
		t.Fatal(err)
	}
	parsed = struct {
		Count  int               `json:"count"`
		Events []json.RawMessage `json:"events"`
	}{}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Count != 2 || !strings.Contains(string(parsed.Events[0]), `"code":-32001`) || !strings.Contains(string(parsed.Events[1]), `"status":"ok"`) {
		t.Fatalf("small-file newest-first broken: %s", got)
	}
}
