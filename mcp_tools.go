package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxToolsFrame bounds one stdin frame, so an oversized frame cannot make the
// tools server allocate without bound. It reuses the HTTP body bound.
const maxToolsFrame = maxHTTPFrame

// frameLimitReader counts the bytes read since the last reset, so one frame is
// capped. The caller resets n before each decode.
type frameLimitReader struct {
	r io.Reader
	n int
}

var errFrameTooLarge = errors.New("tools frame too large")

// cacheDirName is the single temp folder for cache and System One scratch files.
const cacheDirName = "walsy-system1-cache"

func cacheDir() string { return filepath.Join(os.TempDir(), cacheDirName) }

// prepareCacheDir wipes only the dedicated cache folder before an MCP stdin
// transport starts. A failed wipe is reported but never blocks cold start.
func prepareCacheDir(stderr io.Writer) {
	dir := cacheDir()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(stderr, "cache remove:", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(stderr, "cache mkdir:", err)
	}
}

func (l *frameLimitReader) Read(p []byte) (int, error) {
	if l.n >= maxToolsFrame {
		return 0, errFrameTooLarge
	}
	if len(p) > maxToolsFrame-l.n {
		p = p[:maxToolsFrame-l.n]
	}
	n, err := l.r.Read(p)
	l.n += n
	return n, err
}

// runToolsServer is walsy-system1's own MCP stdio server: it exposes the
// dashboard state and the relay audit log as two read-only tools, so the proxy
// is a visible MCP entry instead of only wrapping someone else's server.
func runToolsServer(in io.Reader, out io.Writer) {
	prepareCacheDir(os.Stderr)
	lr := &frameLimitReader{r: in}
	dec := json.NewDecoder(lr)
	enc := json.NewEncoder(out)
	for {
		lr.n = 0
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
		base["result"] = map[string]any{"tools": []any{toolStatusDef, toolAuditDef, toolDecideDef}}
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

var toolDecideDef = map[string]any{
	"name":        "system1_decide",
	"description": "Ask the decision engine one registry item (decision_items.json id) with a state text. Returns the typed label, confidence, admission, the item's proceed labels and the action the caller must take. gate:false items answer locally with no model call; irreversible items admit only a proceed label. Never returns key material or the raw state.",
	"inputSchema": map[string]any{"type": "object", "required": []any{"item", "state"}, "properties": map[string]any{
		"item":   map[string]any{"type": "string", "description": "decision_items.json item id, e.g. intake.intent"},
		"state":  map[string]any{"type": "string", "description": "decision state text"},
		"phase":  map[string]any{"type": "string"},
		"strict": map[string]any{"type": "boolean", "description": "require confidence strictly above the item threshold (default: at or above)"},
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
	case "system1_decide":
		var a struct {
			Item   string `json:"item"`
			State  string `json:"state"`
			Phase  string `json:"phase"`
			Strict bool   `json:"strict"`
		}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
		}
		if a.Item == "" || a.State == "" {
			return toolsError("item and state are required"), nil
		}
		return toolsDecide(a.Item, a.State, a.Phase, a.Strict), nil
	}
	return "", errors.New("unknown tool")
}

// decideReply is the one reply shape; confidence 0 and empty strings are absent.
type decideReply struct {
	Item          string   `json:"item"`
	Label         string   `json:"label,omitempty"`
	Confidence    float64  `json:"confidence,omitempty"`
	Admitted      bool     `json:"admitted"`
	ProceedLabels []string `json:"proceed_labels,omitempty"`
	Action        string   `json:"action,omitempty"`
	Steer         string   `json:"steer,omitempty"`
	Local         bool     `json:"local,omitempty"`
}

func toolsError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// toolsDecide runs one registry item through the relay's SO path. route() is
// not reused because routeResult keeps only a coarse confidence bucket (never
// the label or confidence this tool must return) and the shared decision cache
// lives on decisionBridge, which this server does not hold; so the same pieces
// route() calls - buildStateWithCapability, item.question(), askSOQ,
// item.admits, defaultThreshold - are composed here with route's admission rule.
func toolsDecide(item, state, phase string, strict bool) string {
	it, ok := decisionItemFor(item)
	if !ok {
		return toolsError("unknown item")
	}
	if !it.Gate {
		b, _ := json.Marshal(decideReply{Item: item, Local: true, Steer: "decide locally, no model call"})
		return string(b)
	}
	req, _ := json.Marshal(map[string]any{"state": state, "phase": phase, "item": item})
	thr := it.threshold(defaultThreshold())
	d := askSOQ(buildSOState(string(req)), it.question())
	if d.TimedOut {
		return toolsError("model timeout")
	}
	if d.Choice == "" {
		return toolsError("model unavailable or answer outside the item's label set")
	}
	admitted := it.admits(d.Choice) && aboveThreshold(d.Conf, thr, strict)
	reply := decideReply{
		Item: item, Label: d.Choice, Confidence: d.Conf, Admitted: admitted,
		ProceedLabels: it.Proceed, Action: it.Action[d.Choice],
	}
	if !admitted {
		reply.Steer = steerText(it, d.Choice, strict)
	}
	b, _ := json.Marshal(reply)
	return string(b)
}

// aboveThreshold mirrors route()'s cut; strict turns >= into >.
func aboveThreshold(conf, threshold float64, strict bool) bool {
	if strict {
		return conf > threshold
	}
	return conf >= threshold
}

// steerText is what the caller must do when the label is not admitted.
func steerText(it decisionItem, label string, strict bool) string {
	if s := it.Action[label]; s != "" {
		return s
	}
	if !it.admits(label) {
		return "label outside the proceed list; decide locally or hand to a human"
	}
	if strict {
		return "confidence not strictly above threshold; re-check the state or decide locally"
	}
	return "below confidence threshold; re-check the state or decide locally"
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
