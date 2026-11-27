package app

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

// releaseAliases are the operator-facing release names exposed on MCP surfaces.
const releaseAliases = "walsy-system1, systemone, system one, Smart Router (System One)"

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
// dashboard state and the relay audit log as read-only tools plus the gated
// system1_decide tool, so the proxy is a visible MCP entry instead of only
// wrapping someone else's server.
func runToolsServer(in io.Reader, out io.Writer) {
	lr := &frameLimitReader{r: in}
	dec := json.NewDecoder(lr)
	enc := json.NewEncoder(out)
	d := newDecisionBridge()
	// shortcut: full cache resets at 1024 keys; raise when sessions outlive that.
	const replayMax = 1024
	replies := make(map[string]map[string]any, replayMax)
	for {
		lr.n = 0
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		idemKey := ""
		if req.Method == "tools/call" {
			idemKey = string(req.ID) + "\x00" + string(req.Params)
			if cached, ok := replies[idemKey]; ok {
				_ = enc.Encode(cached)
				continue
			}
		}
		r, dec := d.decideFrameDetailed(toolsGateFrame(raw, req))
		if r.Reason == "no-applicable-question" && dec.Reason == "" {
			// Bucket-only route results carry no decision; pass the typed reason
			// so the tool can name the local refusal instead of a model failure.
			dec.Reason = r.Reason
		}
		reply := toolsReply(req.ID, req.Method, req.Params, d, dec)
		if idemKey != "" {
			if len(replies) >= replayMax {
				replies = make(map[string]map[string]any, replayMax)
			}
			replies[idemKey] = reply
		}
		_ = enc.Encode(reply)
	}
}

// toolsGateFrame lifts the declared system1_decide inputs out of arguments so
// the single frame-level decide hook sees the same state the tool answers with.
// Invoking that gated tool is itself the explicit opt-in, as in toolsDecide's
// direct-call path; no consent is inferred for any other method.
func toolsGateFrame(raw []byte, req struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}) []byte {
	if req.Method != "tools/call" || len(req.Params) == 0 {
		return raw
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &call) != nil || call.Name != "system1_decide" || len(call.Arguments) == 0 {
		return raw
	}
	var args, params map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &args) != nil || json.Unmarshal(req.Params, &params) != nil {
		return raw
	}
	for _, k := range []string{"item", "state", "phase"} {
		if v, ok := args[k]; ok && len(v) > 0 {
			params[k] = v
		}
	}
	if _, ok := params["phase"]; !ok {
		params["phase"] = json.RawMessage(`"before-model"`)
	}
	for _, k := range []string{"need_label", "no_scratch", "safe", "reversible", "authority", "calibrated", "opt_in"} {
		if _, ok := params[k]; !ok {
			params[k] = json.RawMessage("true")
		}
	}
	pb, err := json.Marshal(params)
	if err != nil {
		return raw
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(raw, &env) != nil {
		return raw
	}
	env["params"] = pb
	b, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return b
}

func toolsReply(id json.RawMessage, method string, params json.RawMessage, d *decisionBridge, frameDec ...soDecision) map[string]any {
	base := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		base["result"] = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "walsy-system1", "version": "0.1.0"},
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
		text, err := callTool(call.Name, call.Arguments, d, frameDec...)
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
	"description": "Ask the decision engine one registry item (decision_items.json id) with a state text. Returns the typed label, confidence, admission, the item's proceed labels and the action the caller must take. gate:false items answer locally with no model call; irreversible items admit only a proceed label. Never returns key material or the raw state. Release aliases: " + releaseAliases + ".",
	"inputSchema": map[string]any{"type": "object", "required": []any{"item", "state"}, "properties": map[string]any{
		"item":   map[string]any{"type": "string", "description": "decision_items.json item id, e.g. intake.intent"},
		"state":  map[string]any{"type": "string", "description": "decision state text"},
		"phase":  map[string]any{"type": "string"},
		"strict": map[string]any{"type": "boolean", "description": "require confidence strictly above the item threshold (default: at or above)"},
	}},
}

func callTool(name string, args json.RawMessage, d *decisionBridge, frameDec ...soDecision) (string, error) {
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
		return toolsDecideWithBridge(a.Item, a.State, a.Phase, a.Strict, d, frameDec...), nil
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

// toolsDecide runs one registry item through the single decision hub. The raw
// SO answer rides on routeResult.LastDecision; routeResult's bucket alone is
// too coarse for the label/confidence this tool must return.
func toolsDecide(item, state, phase string, strict bool, frameDec ...soDecision) string {
	return toolsDecideWithBridge(item, state, phase, strict, newDecisionBridge(), frameDec...)
}

// toolsDecideWithBridge keeps one cache for the lifetime of an MCP session; a
// direct call without a session still gets a fresh bridge.
func toolsDecideWithBridge(item, state, phase string, strict bool, bridge *decisionBridge, frameDec ...soDecision) string {
	it, ok := decisionItemFor(item)
	if !ok {
		return toolsError("unknown item")
	}
	if !it.Gate {
		b, _ := json.Marshal(decideReply{Item: item, Local: true, Steer: "decide locally, no model call"})
		return string(b)
	}
	if phase == "" {
		phase = "before-model"
	}
	thr := it.threshold(defaultThreshold())
	var d soDecision
	if len(frameDec) > 0 {
		// One decide hook per frame: reuse the typed SO answer already produced
		// by decideFrameDetailed; a second call would double the SO POSTs.
		d = frameDec[0]
	} else {
		// Direct callers without a frame-level decision keep the old path.
		req, _ := json.Marshal(map[string]any{
			"state": state, "phase": phase, "item": item, "method": "tools/call",
			"need_label": true, "no_scratch": true, "safe": true, "reversible": true,
			"authority": true, "calibrated": true, "opt_in": true,
		})
		_, d = bridge.decideFull(req)
	}
	if d.TimedOut {
		return toolsError("model timeout")
	}
	if d.Choice == "" {
		if d.Reason == "no-applicable-question" {
			return toolsError("question inapplicable: missing required state fields")
		}
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
	path := filepath.Join(home, routerDir, routerAuditName)
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return `{"count":0,"events":[]}`, nil
		}
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return "", fmt.Errorf("audit path %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tail := st.Size() > int64(maxToolsFrame)
	if tail {
		if _, err := f.Seek(-int64(maxToolsFrame), io.SeekEnd); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if tail {
		// Seek starts mid-line; only lines after the first newline are whole.
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
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
