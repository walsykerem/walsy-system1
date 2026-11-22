package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// opencodeMCPSnippet returns the JSON for mcp.servers.<name> that wires this
// binary as a local stdio wrapper in front of an upstream MCP server.
// Only process wiring is emitted: no keys, tokens or other secrets.
func opencodeMCPSnippet(name string, upstream []string) string {
	if name == "" {
		name = "system1"
	}
	binary, err := os.Executable()
	if err != nil || !filepath.IsAbs(binary) {
		binary = "walsy-system1"
	}
	command := make([]string, 0, len(upstream)+3)
	command = append(command, binary, "-transport", "stdio")
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
