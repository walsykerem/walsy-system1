package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Guards the harness itself: a PASS can only come from a real forwarded echo.
func TestCheckForwardRejectsBlockAndAcceptsEcho(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want int
	}{
		{"blocked", map[string]any{"error": map[string]any{"message": "blocked: no"}}, 1},
		{"wrong method", map[string]any{"method": "initialize", "params": map[string]any{"path": "/x"}}, 1},
		{"args not forwarded", map[string]any{"method": "tools/call", "params": map[string]any{"path": "/y"}}, 1},
		{"nil", nil, 1},
		{"echoed", map[string]any{"method": "tools/call", "params": map[string]any{"path": "/x"}}, 0},
	}
	for _, c := range cases {
		fail = 0
		checkForward(c.name, c.m, "/x")
		if fail != c.want {
			t.Fatalf("%s: fail=%d want %d", c.name, fail, c.want)
		}
	}
	fail = 0
}

// An empty assertion must fail instead of printing PASS for nothing.
func TestCheckWithoutWantedTextFails(t *testing.T) {
	fail = 0
	check("vacuous", "anything")
	if fail != 1 {
		t.Fatalf("empty want: fail=%d want 1", fail)
	}
	fail = 0
}

// checkReason must read the reason field the gate recorded, and must fail
// rather than pass when the log is missing or carries a different code.
func TestCheckReasonReadsAuditField(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".walsy-system1"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, ".walsy-system1", "audit.jsonl")
	body := "{\"event\":\"decision\",\"decision\":\"pass\",\"reason\":\"layer0-static\"}\n"
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fail = 0
	checkReason("present", home, "layer0-static")
	if fail != 0 {
		t.Fatalf("present reason: fail=%d want 0", fail)
	}
	checkReason("absent", home, "so-no")
	if fail != 1 {
		t.Fatalf("absent reason: fail=%d want 1", fail)
	}
	fail = 0
	checkReason("no log", t.TempDir(), "so-no")
	if fail != 1 {
		t.Fatalf("missing log: fail=%d want 1", fail)
	}
	fail = 0
}
