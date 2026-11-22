package main

import "testing"

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
