package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// opencodeMCPSnippet returns the JSON for mcp.servers.<name> that wires this
// binary with the requested transport. stdio needs an upstream command to
// proxy, so it falls back to the self-contained tools server instead of
// printing a command that dies at startup.
// Named -listen/-upstream values are emitted before positional upstream args;
// empty values are suppressed. Only process wiring is emitted: no keys, tokens
// or other secrets.
func opencodeMCPSnippet(name, transport, listen, upstreamAddr string, upstream []string) string {
	if name == "" {
		name = "system1"
	}
	if transport == "" {
		transport = "stdio"
	}
	if transport == "stdio" && len(upstream) == 0 {
		transport = "tools"
	}
	binary, err := os.Executable()
	if err != nil || !filepath.IsAbs(binary) {
		binary = "walsy-system1"
	}
	command := make([]string, 0, len(upstream)+7)
	command = append(command, binary, "-transport", transport)
	if listen != "" {
		command = append(command, "-listen", listen)
	}
	if upstreamAddr != "" {
		command = append(command, "-upstream", upstreamAddr)
	}
	command = append(command, upstream...)
	b, err := json.Marshal(map[string]any{
		"mcp": map[string]any{
			"servers": map[string]any{
				name: map[string]any{
					"type":    "local",
					"command": command,
				},
			},
		},
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}
