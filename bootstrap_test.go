package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type bootstrapSnippet struct {
	MCP struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
		} `json:"servers"`
	} `json:"mcp"`
}

func parseBootstrapSnippet(t *testing.T, raw string) bootstrapSnippet {
	t.Helper()
	var got bootstrapSnippet
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("snippet is not JSON: %v (%s)", err, raw)
	}
	return got
}

func TestBootstrapOpenCodeMCPSnippet(t *testing.T) {
	raw := opencodeMCPSnippet("system1", "stdio", []string{"--flag", "value"})
	if strings.Contains(strings.ToLower(raw), "environment") ||
		strings.Contains(strings.ToLower(raw), "token") ||
		strings.Contains(raw, "sk-") || strings.Contains(raw, testSecret) {
		t.Fatalf("snippet leaked a secret or env block: %s", raw)
	}
	got := parseBootstrapSnippet(t, raw)
	srv, ok := got.MCP.Servers["system1"]
	if !ok {
		t.Fatalf("missing mcp.servers.system1: %s", raw)
	}
	if srv.Type != "local" {
		t.Fatalf("type = %q, want local", srv.Type)
	}
	want := []string{"-transport", "stdio", "--flag", "value"}
	if len(srv.Command) != len(want)+1 {
		t.Fatalf("command = %v, want binary + %v", srv.Command, want)
	}
	bin := srv.Command[0]
	if !filepath.IsAbs(bin) && bin != "walsy-system1" {
		t.Fatalf("command[0] = %q, want absolute binary path or walsy-system1", bin)
	}
	for i, v := range want {
		if srv.Command[i+1] != v {
			t.Fatalf("command[%d] = %q, want %q", i+1, srv.Command[i+1], v)
		}
	}

	// stdio cannot run without an upstream command, so the snippet must point
	// at the self-contained tools server instead of a dead invocation.
	raw = opencodeMCPSnippet("", "", nil)
	got = parseBootstrapSnippet(t, raw)
	srv, ok = got.MCP.Servers["system1"]
	if !ok {
		t.Fatalf("empty name must default to system1: %s", raw)
	}
	if len(srv.Command) != 3 || srv.Command[1] != "-transport" || srv.Command[2] != "tools" {
		t.Fatalf("empty-upstream command = %v, want -transport tools", srv.Command)
	}

	raw = opencodeMCPSnippet("system1", "tools", nil)
	got = parseBootstrapSnippet(t, raw)
	if cmd := got.MCP.Servers["system1"].Command; len(cmd) != 3 || cmd[2] != "tools" {
		t.Fatalf("tools transport not honored: %v", cmd)
	}
}

func TestBootstrapSnippetRoundTripJSON(t *testing.T) {
	raw := opencodeMCPSnippet("upstream", "stdio", []string{"npx", "-y", "@example/mcp"})
	got := parseBootstrapSnippet(t, raw)
	cmd := got.MCP.Servers["upstream"].Command
	if len(cmd) != 6 || cmd[len(cmd)-3] != "npx" || cmd[len(cmd)-2] != "-y" || cmd[len(cmd)-1] != "@example/mcp" {
		t.Fatalf("upstream args not preserved: %v", cmd)
	}
}
