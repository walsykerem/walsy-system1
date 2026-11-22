package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Decision wiring: the stdio/unix relays call the real engine on the JSON-RPC
// stream. Advisory mode is forward-first and never delays a frame; enforcing
// mode gates initialize/tools/call before forwarding and blocks only on a
// high-confidence SO deny. Every SO absence/error/degradation fails open.
// Never log raw payload, secret, conf, token or cost.

type decisionBridge struct {
	mu     sync.Mutex
	cache  *decisionCache
	pin    *pin
	keySet bool
	marker sync.Once
	guard  *writeGuard
}

func newDecisionBridge() *decisionBridge {
	keySet := soAPIKey() != "" // presence only, never logged or forwarded to client
	list, err := loadRegistry()
	if err != nil {
		list = bakedRegistry()
	}
	return &decisionBridge{cache: newDecisionCache(), pin: newPin(list), keySet: keySet,
		guard: newWriteGuard()}
}

// gateKey binds a decision to request bytes and capability config. Budget is
// deliberately absent: spending the budget must block the call, not mint a new
// cache cell for the same request.
func gateKey(request []byte, cap capabilityConfig) string {
	h := sha256.New()
	_, _ = h.Write(request)
	b, _ := json.Marshal(cap)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// buildStateWithCapability fills missing opt-in fields from the built-in
// defaults so a real client (which sends none of these fields) reaches SO.
func buildStateWithCapability(request string, cap capabilityConfig) string {
	var obj map[string]any
	if json.Unmarshal([]byte(request), &obj) != nil || obj == nil {
		obj = map[string]any{}
	}
	if cap.NeedLabel {
		if _, ok := obj["need_label"].(bool); !ok {
			obj["need_label"] = true
		}
	}
	if cap.NoScratch {
		if _, ok := obj["no_scratch"].(bool); !ok {
			obj["no_scratch"] = true
		}
	}
	if cap.Safe {
		if _, ok := obj["safe"].(bool); !ok {
			obj["safe"] = true
		}
	}
	if cap.Reversible {
		if _, ok := obj["reversible"].(bool); !ok {
			obj["reversible"] = true
		}
	}
	if cap.Authority {
		if _, ok := obj["authority"].(bool); !ok {
			obj["authority"] = true
		}
	}
	if cap.Calibrated {
		if _, ok := obj["calibrated"].(bool); !ok {
			obj["calibrated"] = true
		}
	}
	if cap.Phase != "" {
		if s, ok := obj["phase"].(string); !ok || s == "" {
			obj["phase"] = cap.Phase
		}
	}
	for _, k := range []string{"need_label", "no_scratch", "safe", "reversible", "authority", "calibrated"} {
		if _, ok := obj[k].(bool); !ok {
			obj[k] = true
		}
	}
	if s, ok := obj["phase"].(string); !ok || s == "" {
		obj["phase"] = "before-model"
	}
	obj["opt_in"] = true
	// Make the state gate-shaped so SO is reachable; never invent content.
	s, _ := obj["state"].(string)
	if !isLabelShaped(s) {
		obj["state"] = "decide: " + s
	}
	b, _ := json.Marshal(obj)
	return buildState(string(b))
}

func cacheResult(r routeResult) cachedDecision {
	if r.Decision == decPin {
		return cachedDecision{Choice: "yes", ConfBucket: r.ConfBucket, Reason: "margin=1"}
	}
	return cachedDecision{Choice: abstainLabel, ConfBucket: r.ConfBucket, Reason: r.Reason}
}

func cachedResult(c cachedDecision) routeResult {
	if c.Choice == "yes" {
		return routeResult{Decision: decPin, Reason: "cache-hit", Covered: true, ConfBucket: c.ConfBucket}
	}
	return passResult("cache-"+c.Reason, false)
}

// decide runs buildState + route per request, keyed by request+capability.
// No one-shot latch: a second distinct frame gets a fresh decision.
func (d *decisionBridge) decide(request []byte) routeResult {
	d.mu.Lock()
	keySet := d.keySet
	d.mu.Unlock()
	if keySet {
		d.marker.Do(func() {
			emitMetric(metric{Seq: 1, PID: os.Getpid(), Route: markerKey, Reason: "key_set"})
		})
	}
	if !keySet {
		return passResult("no-key", false)
	}
	cap := soCapability()
	state := buildStateWithCapability(string(request), cap)
	key := gateKey(request, cap)
	if cached, hit := d.cache.lookup(key); hit {
		return cachedResult(cached)
	}
	r := route(state, d.pin, nil)
	d.cache.store(key, cacheResult(r))
	return r
}

// unwrapParams returns the params object of a JSON-RPC envelope so buildState
// sees the opt-in fields instead of the envelope's method/id keys.
func unwrapParams(raw []byte, f frame) []byte {
	if len(f.Params) > 0 {
		var env map[string]json.RawMessage
		if json.Unmarshal(raw, &env) == nil && len(env["params"]) > 0 {
			return []byte(env["params"])
		}
	}
	return raw
}

func toolName(params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return scrubText(p.Name)
}

type rpcErrorPayload struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpcError is a valid JSON-RPC error response - never an HTTP 503.
func rpcError(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	p := rpcErrorPayload{JSONRPC: "2.0", ID: id}
	p.Error.Code, p.Error.Message = code, msg
	b, _ := json.Marshal(p)
	return append(b, '\n')
}

// enforcingGate inspects before forwarding. A high-confidence SO deny or a
// spent session budget blocks; a missing router policy still fails open.
func (d *decisionBridge) enforcingGate(request []byte) ([]byte, bool) {
	f, err := decodeFrame(request)
	if err != nil || (f.Method != "initialize" && f.Method != "tools/call") {
		return nil, false
	}
	tool := toolName(f.Params)
	if f.Method == "initialize" {
		// The documented budget boundary: a new MCP session starts a new budget.
		routerBudgetReset()
	}
	if f.Method == "tools/call" {
		if reason, blocked := d.guard.check(tool, toolArgs(f.Params)); blocked {
			routerAudit("writeguard", "tool", tool, "reason", reason)
			return rpcError(f.ID, -32001, "blocked: "+reason), true
		}
		if body, ok := writeContent(toolArgs(f.Params)); ok {
			p := argumentPath(toolArgs(f.Params))
			// Security precheck over the text this call would add, before it lands.
			if v := diffSecurityCheck(p, oldContentOnDisk(p), body); v.Refuse {
				routerAudit("diffguard", "tool", tool, "reason", v.Code)
				return rpcError(f.ID, -32001, "blocked: "+v.Steer), true
			}
		} else if v := argsSecretCheck(toolArgs(f.Params)); v.Refuse {
			// No file body to diff: a shell redirect or an unknown schema still
			// cannot carry a credential shape through this hop.
			routerAudit("diffguard", "tool", tool, "reason", v.Code)
			return rpcError(f.ID, -32001, "blocked: "+v.Steer), true
		}
		if _, max, spent := routerBudgetSpend(); !spent {
			routerAudit("budget", "tool", tool, "budget", "exhausted",
				"count", strconv.Itoa(max))
			return rpcError(f.ID, -32001, budgetSteer(max)), true
		}
	}
	allowed := routerAllow(tool)
	routerAudit("gate", "method", f.Method, "tool", tool,
		"allow", strconv.FormatBool(allowed))
	r := d.decide(unwrapParams(request, f))
	routerAudit("decision", "method", f.Method, "decision", r.Decision, "reason", r.Reason)
	if r.Decision == decDeny {
		return rpcError(f.ID, -32000, withSteer("blocked by decision gate", r.Steer)), true
	}
	// Enforcing only, and only when the decision is not an admit: an admitted
	// item's Steer is its own "continue" action, not a refusal to relay.
	if soMode() == "enforcing" && steerBlocks(r) {
		return rpcError(f.ID, -32001, withSteer("blocked", r.Steer)), true
	}
	return nil, false
}

func withSteer(base, steer string) string {
	if steer == "" {
		return base
	}
	return base + ": " + steer
}

// budgetSteer is the exhausted-budget refusal body: it names the budget, its
// refill boundary and the alternative, so the caller is never stalled blind.
func budgetSteer(max int) string {
	return withSteer("blocked", "router tool budget spent ("+strconv.Itoa(max)+
		" calls for this session); reset: send initialize for a new session or raise WALSY_ROUTER_RETRY_BUDGET; alternative: finish with what is in context")
}

// steerBlocks: an admitted decision's Steer is its own action ("continue"),
// so only a refusal that names a next step blocks the relay. Transport and
// scope failures stay fail-open: a Steer there is advice, not a verdict.
func steerBlocks(r routeResult) bool {
	return r.Decision != decPin && r.Steer != "" && !steerFailOpen[r.Reason]
}

// steerFailOpen reasons are advice, not verdicts: the decision model was
// unreachable or the request was out of scope, so the relay keeps forwarding.
var steerFailOpen = map[string]bool{
	"transient": true, "permanent": true, "unknown": true,
	"no-key": true, "no-state": true, "-32602": true,
	"layer0-scope": true, "layer0-gate": true, "layer0-static": true,
	"layer0-shape": true, "registry-local": true,
	"so-degraded": true, "so-admit": true, "so-no": true, "so-other": true, "cache-hit": true,
	"ok": true, "ledger-complete": true,
}

// advisoryHooks record router canary decisions after forwarding and mirror a
// scrubbed copy when a mirror is configured. The mirror response is ignored.
func (d *decisionBridge) advisoryHooks(request []byte) {
	f, err := decodeFrame(request)
	if err != nil {
		return
	}
	tool := toolName(f.Params)
	if tool == "" {
		return
	}
	// Advisory is fail-open: a guard hit is recorded only, so the default mode
	// still shows which writes enforcing mode would have refused.
	if f.Method == "tools/call" {
		if reason, hit := d.guard.check(tool, toolArgs(f.Params)); hit {
			routerAudit("writeguard", "mode", "advisory",
				"tool", tool, "reason", guardReasonCode(reason))
		}
		if body, ok := writeContent(toolArgs(f.Params)); ok {
			p := argumentPath(toolArgs(f.Params))
			if v := diffSecurityCheck(p, oldContentOnDisk(p), body); v.Refuse {
				routerAudit("diffguard", "mode", "advisory", "tool", tool, "reason", v.Code)
			}
		}
	}
	allowed := routerAllow(tool)
	// Advisory is fail-open: no retry token is spent in the default path.
	routerRecord(tool, allowed)
	if mirror := routerMirrorURL(); mirror != "" && allowed {
		mirrorCanary(mirror, request)
	}
	routerAudit("canary", "method", f.Method, "tool", tool)
}

func scrubbedMirror(request []byte) []byte {
	var obj any
	if json.Unmarshal(request, &obj) != nil {
		return []byte(`{}`)
	}
	b, _ := json.Marshal(cleanUser(obj))
	return b
}

func mirrorCanary(mirrorURL string, request []byte) {
	body := scrubbedMirror(request)
	go func() {
		req, err := http.NewRequest(http.MethodPost, mirrorURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
}

// bridgeStdio is the fail-safe stdio interceptor. Advisory client frames are
// forwarded byte-for-byte before any decision work; enforcing frames are
// gated first. Initialize results get the MCP marker added.
func bridgeStdio(clientIn io.Reader, clientOut io.Writer, upstreamIn io.Writer, upstreamOut io.Reader) {
	d := newDecisionBridge()
	enforcing := soMode() == "enforcing"
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(clientIn)
		for {
			frame, err := br.ReadBytes('\n')
			if len(frame) > 0 {
				relayLog("client", frame)
				if enforcing {
					if resp, blocked := d.enforcingGate(frame); blocked {
						_, _ = clientOut.Write(resp)
						if err != nil {
							break
						}
						continue
					}
				}
				// Forward first in advisory: never hold a client frame hostage.
				_, _ = upstreamIn.Write(frame)
				if !enforcing {
					d.inspectClient(frame)
					d.advisoryHooks(frame)
				}
			}
			if err != nil {
				break
			}
		}
		halfCloseWriter(upstreamIn)
	}()
	br := bufio.NewReader(upstreamOut)
	for {
		frame, err := br.ReadBytes('\n')
		if len(frame) > 0 {
			relayLog("server", frame)
			_, _ = clientOut.Write(d.inspectServer(frame))
		}
		if err != nil {
			break
		}
	}
	<-done // don't return until client frames are fully forwarded
}

func (d *decisionBridge) inspectClient(frame []byte) {
	f, err := decodeFrame(frame)
	if err != nil || f.Method != "initialize" {
		return
	}
	decision := unwrapParams(frame, f)
	_ = d.decide(decision)
}

func (d *decisionBridge) inspectServer(frame []byte) []byte {
	f, err := decodeFrame(frame)
	if err != nil || !isInitializeResult(f) {
		return frame
	}
	// addRouteCapability rewrites the result payload span. Splice it back
	// into the envelope, keeping id/protocolVersion and all other bytes.
	newResult := addRouteCapability(f.Result)
	if bytes.Equal(newResult, f.Result) {
		return frame
	}
	prefix := []byte(`"result":`)
	idx := bytes.Index(frame, prefix)
	if idx < 0 {
		return frame
	}
	start := idx + len(prefix)
	end := start + len(f.Result)
	if end > len(frame) || !bytes.Equal(frame[start:end], f.Result) {
		return frame
	}
	out := make([]byte, 0, len(frame)+len(newResult)-len(f.Result))
	out = append(out, frame[:start]...)
	out = append(out, newResult...)
	out = append(out, frame[end:]...)
	return out
}

// halfCloseWriter signals EOF on the write side only; the upstream may still
// flush a reply on its stdout.
func halfCloseWriter(w io.Writer) {
	if c, ok := w.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
		return
	}
	if c, ok := w.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}
