package app

import (
	"encoding/json"
	"os"
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

// -print-client-config is the shipped CLI entry for client wiring, so the
// transport matrix is checked through Main instead of the snippet helper.
func TestBootstrapPrintClientConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldArgs := os.Args
	t.Cleanup(func() {
		os.Args = oldArgs
		*flagTransport, *flagListen, *flagUpstream, *flagPrintConfig = "stdio", "", "", false
	})
	cases := []struct {
		name   string
		args   []string
		server string
		want   []string
	}{
		{
			name: "default stdio without upstream falls back to tools",
			args: []string{"walsy-system1", "-print-client-config"},
			want: []string{"-transport", "tools"},
		},
		{
			name: "explicit stdio keeps positional upstream argv",
			args: []string{"walsy-system1", "-transport", "stdio", "-print-client-config", "npx", "@example/mcp"},
			want: []string{"-transport", "stdio", "npx", "@example/mcp"},
		},
		{
			name: "unix listen and upstream",
			args: []string{"walsy-system1", "-transport", "unix", "-listen", "/tmp/s1.sock", "-upstream", "/tmp/up.sock", "-print-client-config"},
			want: []string{"-transport", "unix", "-listen", "/tmp/s1.sock", "-upstream", "/tmp/up.sock"},
		},
		{
			name: "http listen",
			args: []string{"walsy-system1", "-transport", "http", "-listen", "127.0.0.1:8787", "-print-client-config"},
			want: []string{"-transport", "http", "-listen", "127.0.0.1:8787"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*flagTransport, *flagListen, *flagUpstream, *flagPrintConfig = "stdio", "", "", false
			os.Args = tc.args

			raw, _ := gapMainCapture(t, Main)
			raw = strings.TrimSpace(raw)
			if strings.Contains(strings.ToLower(raw), "environment") ||
				strings.Contains(strings.ToLower(raw), "token") ||
				strings.Contains(raw, "sk-") || strings.Contains(raw, testSecret) {
				t.Fatalf("snippet leaked a secret or env block: %s", raw)
			}

			got := parseBootstrapSnippet(t, raw)
			srv, ok := got.MCP.Servers["walsy-system1"]
			if !ok {
				t.Fatalf("missing mcp.servers.walsy-system1: %s", raw)
			}
			if srv.Type != "local" {
				t.Fatalf("type = %q, want local", srv.Type)
			}
			if len(srv.Command) != len(tc.want)+1 {
				t.Fatalf("command = %v, want binary + %v", srv.Command, tc.want)
			}
			if srv.Command[0] == "" {
				t.Fatalf("command[0] is empty: %v", srv.Command)
			}
			for i, v := range tc.want {
				if srv.Command[i+1] != v {
					t.Fatalf("command[%d] = %q, want %q (command %v)", i+1, srv.Command[i+1], v, srv.Command)
				}
			}
		})
	}
}
