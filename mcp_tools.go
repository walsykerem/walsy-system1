package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runToolsServer is walsy-system1's own MCP stdio server: it exposes the
// dashboard state and the relay audit log as two read-only tools, so the proxy
// is a visible MCP entry instead of only wrapping someone else's server.
func runToolsServer(in io.Reader, out io.Writer) {
	dec := json.NewDecoder(bufio.NewReader(in))
	enc := json.NewEncoder(out)
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		_ = enc.Encode(toolsReply(req.ID, req.Method, req.Params))
	}
}

func toolsReply(id json.RawMessage, method string, params json.RawMessage) map[string]any {
	base := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		base["result"] = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "walsy-system1", "version": "1.0.0"},
		}
	case "tools/list":
		base["result"] = map[string]any{"tools": []any{toolStatusDef, toolAuditDef}}
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			base["error"] = map[string]any{"code": -32602, "message": "invalid params"}
			return base
		}
		text, err := callTool(call.Name, call.Arguments)
		if err != nil {
			base["error"] = map[string]any{"code": -32602, "message": err.Error()}
			return base
		}
		base["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	case "ping":
		base["result"] = map[string]any{}
	default:
		base["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	return base
}

var toolStatusDef = map[string]any{
	"name":        "system1_status",
	"description": "Provider names, stored-key presence, models, combo steps and build target of the local walsy-system1 dashboard. Never returns key values.",
	"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
}

var toolAuditDef = map[string]any{
	"name":        "system1_audit",
	"description": "Newest-first tail of the relay audit log: frame direction, method, status, size. errorsOnly keeps malformed frames and JSON-RPC error statuses.",
	"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
		"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
		"errorsOnly": map[string]any{"type": "boolean"},
	}},
}

func callTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case "system1_status":
		return toolsStatus()
	case "system1_audit":
		var a struct {
			Limit      int  `json:"limit"`
			ErrorsOnly bool `json:"errorsOnly"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
		}
		return toolsAudit(a.Limit, a.ErrorsOnly)
	}
	return "", errors.New("unknown tool")
}

func toolsStatus() (string, error) {
	view, err := providerView()
	if err != nil {
		return "", err
	}
	models, err := loadModels()
	if err != nil {
		return "", err
	}
	combo, err := loadCombo()
	if err != nil {
		return "", err
	}
	providers := make([]map[string]any, 0, len(view.Providers))
	for _, p := range view.Providers {
		providers = append(providers, map[string]any{
			"name": p.Name, "base": p.Base, "keySet": p.KeySet,
			"models": len(models[p.Name]), "baked": p.Baked,
		})
	}
	steps := make([]map[string]string, 0, len(combo))
	for _, s := range combo {
		steps = append(steps, map[string]string{"provider": s.Provider, "model": s.Model})
	}
	target := ""
	if path, err := buildTargetPath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			target = strings.TrimSpace(string(b))
		}
	}
	out, err := json.Marshal(map[string]any{
		"providers": providers, "combo": steps, "buildTarget": target,
	})
	return string(out), err
}

func isAuditError(status string) bool {
	return status == "malformed" || strings.HasPrefix(status, "error_")
}

func toolsAudit(limit int, errorsOnly bool) (string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		if os.IsNotExist(err) {
			return `{"count":0,"events":[]}`, nil
		}
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	found := make([]json.RawMessage, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(found) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if errorsOnly && !isAuditError(rec.Status) {
			continue
		}
		found = append(found, json.RawMessage(line))
	}
	out, err := json.Marshal(map[string]any{"count": len(found), "events": found})
	return string(out), err
}
