package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Decision wiring: the stdio/unix relays call the real engine on the JSON-RPC
// stream. Advisory mode is forward-first and never delays a frame. Enforcing
// mode gates initialize/tools/call before forwarding: write/diff guards and a
// spent session budget block, and an enforcing-mode steer on a non-admitted
// decision blocks too. Every SO absence/error/degradation fails open; only a
// terminal admit is cached, so a transient failure is retried rather than
// poisoning the cache. Never log raw payload, secret, conf, token or cost.

type decisionBridge struct {
	mu        sync.Mutex
	cache     *shardedCache
	pin       *pin
	keySet    bool
	marker    sync.Once
	guard     *writeGuard
	preflight *preflightCalibration
	chain     *receiptChain
	flight    *soFlightGroup
	oraclePin []byte // RAM-only sha256(endpoint||x00||key), set at session start
}

func newDecisionBridge() *decisionBridge {
	keySet := soAPIKey() != "" // presence only, never logged or forwarded to client
	oraclePin := soOracleIdentity()
	list, err := loadRegistry()
	if err != nil {
		list = bakedRegistry()
	}
	return &decisionBridge{cache: newShardedCache(), pin: newPin(list), keySet: keySet,
		guard: newWriteGuard(), preflight: newPreflightCalibration(), chain: &receiptChain{}, flight: newSOFlightGroup(), oraclePin: oraclePin}
}

const (
	keyRubricVersion = "1"
	keyPolicyVersion = "1"
	keyModel         = "clef"
	keyConfRule      = "conf>=0.8+margin>=0.10"
)

// registryVersion pins the embedded decision registry. A registry change
// deterministically misses the old process-local cache; no explicit flush.
var registryVersion = func() string {
	s := sha256.Sum256(decisionItemsJSON)
	return hex.EncodeToString(s[:])[:8]
}()

// promptSetHash binds the key to the active prompt set without putting prompt
// bodies into SO state. Names are sorted; only digests cross the boundary.
func promptSetHash() string {
	ps := loadPrompts()
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	h := sha256.New()
	idx := make(map[string]Prompt, len(ps))
	for _, p := range ps {
		idx[p.Name] = p
	}
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		s := sha256.Sum256([]byte(idx[name].Text))
		_, _ = h.Write(s[:])
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeField writes a fixed-order length-prefixed byte string so no two field
// sequences can collide by concatenation.
func writeField(h io.Writer, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

// gateKeyBytes binds a decision to a request's canonical form, capability
// config, rubric/policy versions, model, conf-rule, registry digest and prompt
// set. Volatile envelope identifiers are stripped before canonicalState.
// shortcut: exact canonical key only; semantic/embedding tier stays CLOSED,
// reopen only with a measured accuracy-vs-hit sweep.
func gateKeyBytes(request []byte, cap capabilityConfig) [32]byte {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(request))
	dec.UseNumber()
	if dec.Decode(&obj) != nil {
		obj = nil
	}
	if obj != nil {
		for _, k := range []string{"id", "jsonrpc", "method"} {
			delete(obj, k)
		}
	}
	b, _ := json.Marshal(obj)
	h := sha256.New()
	_, _ = h.Write([]byte(canonicalState(string(b))))
	c, _ := json.Marshal(cap)
	writeField(h, string(c))
	writeField(h, keyRubricVersion)
	writeField(h, keyPolicyVersion)
	writeField(h, keyModel)
	writeField(h, keyConfRule)
	writeField(h, registryVersion)
	writeField(h, promptSetHash())
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// gateKey is the string wrapper kept for tests and callers that compare hex.
func gateKey(request []byte, cap capabilityConfig) string {
	k := gateKeyBytes(request, cap)
	return hex.EncodeToString(k[:])
}

// buildSOState prepares the envelope for SO. Consent is never invented: missing
// opt-ins stay absent so buildState reads them false. User prompts are untrusted
// data and stay out of the SO state entirely; they remain in the local UI.
// A null return means the SO-bound skeleton could not be built safely; the
// caller must fail open without a POST (a rune-capped state is never truncated).
func buildSOState(request string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(request), &obj) != nil || obj == nil {
		obj = map[string]any{}
	}
	// Make the state gate-shaped so SO is reachable; never invent content.
	// A bare static word (ping, status, ...) is left untouched so Layer 0
	// answers it for free instead of paying for a model call.
	s, _ := obj["state"].(string)
	if !isLabelShaped(s) && !staticShortCircuit(s) {
		obj["state"] = "decide: " + s
	}
	b, _ := json.Marshal(obj)
	var e stateEnvelope
	if json.Unmarshal([]byte(buildState(string(b))), &e) != nil {
		return ""
	}
	// Prompt bodies never enter this state: only their ids and SHA-256 go on
	// the wire, and the whole payload is rune-capped before any POST.
	state, err := soStateFromEnvelope(e)
	if err != nil || state == "" {
		return ""
	}
	return state
}

// terminalNegative are the reason-coded abstains safe to cache briefly. All
// transient/degraded/no-key/timeout states are deliberately absent so they are
// retried, not frozen into a fail-open cell.
var terminalNegative = map[string]bool{
	"so-low-conf": true, "so-no": true, "so-other": true, "registry-nonproceed": true,
}

// cacheResult projects a terminal route result into the cell the bridge stores.
func cacheResult(r routeResult) cachedDecision {
	if r.Abstained {
		return cachedDecision{Abstain: true, Reason: r.Reason, ConfBucket: r.ConfBucket}
	}
	return cachedDecision{Choice: "yes", ConfBucket: r.ConfBucket, Reason: "margin=1"}
}

// cachedResult never serves an abstain cell as a label or an admit.
func cachedResult(c cachedDecision) routeResult {
	if c.Abstain {
		return routeResult{Decision: decPass, Reason: c.Reason, Abstained: true, ConfBucket: c.ConfBucket}
	}
	return routeResult{Decision: decPin, Reason: "cache-hit", Covered: true, ConfBucket: c.ConfBucket}
}

// cacheKey binds the request key to the engine generation and pinned oracle
// identity. A rotated or swapped-out engine never reads another engine's cell;
// generation is identity, not a TTL.
func (d *decisionBridge) cacheKey(request []byte, cap capabilityConfig, gen uint64) [32]byte {
	base := gateKeyBytes(request, cap)
	h := sha256.New()
	_, _ = h.Write(base[:])
	_ = binary.Write(h, binary.BigEndian, gen)
	_, _ = h.Write(d.oraclePin)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
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
	state := buildSOState(string(request))
	p := preflightFromState(state)
	gen := d.pin.generation()
	key := d.cacheKey(request, cap, gen)
	if cached, hit := d.cache.lookup(key); hit {
		r := cachedResult(cached)
		recordPreflight(p, r, d.preflight)
		admitStats.record(r.Decision == decPin)
		return r
	}
	r := d.flight.do(key, func() routeResult { return route(state, d.pin) })
	// Shadow only: preflight NEVER changes r. Recorded after r is final.
	recordPreflight(p, r, d.preflight)
	admitStats.record(r.Decision == decPin)
	// Terminal admits and short-lived terminal abstains are cached; transient
	// and degraded states never are. A pin that changed during computation must
	// not write its answer under the old engine's key.
	if (r.Decision == decPin || (r.Abstained && terminalNegative[r.Reason])) && d.pin.generation() == gen {
		d.cache.store(key, cacheResult(r))
	}
	return r
}

// unwrapParams returns the params object of a JSON-RPC envelope so buildState
// sees the opt-in fields instead of the envelope's method/id keys. The method
// is injected from the decoded frame, never from client-controlled params.
func unwrapParams(raw []byte, f frame) []byte {
	if len(f.Params) > 0 {
		var env map[string]json.RawMessage
		if json.Unmarshal(raw, &env) == nil && len(env["params"]) > 0 {
			var p map[string]any
			params := []byte(env["params"])
			if json.Unmarshal(params, &p) == nil {
				p["method"] = f.Method
				if b, err := json.Marshal(p); err == nil {
					return b
				}
			}
			return params
		}
	}
	return raw
}

// decideFrame is the single decision hook: decode once, inject the trusted
// method, route in-scope frames through decide, pass everything else through
// without an SO call. Out-of-scope frames are reported, never gated.
func (d *decisionBridge) decideFrame(raw []byte) routeResult {
	// YC8 trust boundary: refuse a 1 MiB+ frame before any parse or SO call.
	if len(raw) >= 1<<20 {
		return passResult("frame-too-large", false)
	}
	f, err := decodeFrame(raw)
	if err != nil {
		return passResult("no-state", false)
	}
	m := strings.ToLower(f.Method)
	if m != "initialize" && m != "tools/call" {
		routerAudit("gate", "reason", "layer0-scope", "method", f.Method)
		return passResult("layer0-scope", false)
	}
	r := d.decide(unwrapParams(raw, f))
	d.emitReceipt(r, f.Method, toolName(f.Params))
	return r
}

// receiptWorthy excludes named Layer 0/static outcomes: they are answered
// locally, so chaining them as SO decisions would misrepresent the source.
func receiptWorthy(r routeResult) bool {
	if r.Passthrough || strings.HasPrefix(r.Reason, "layer0-") {
		return false
	}
	return r.Reason != "registry-local" && r.Reason != "ledger-complete"
}

// emitReceipt appends a redacted, chained receipt for one in-scope request.
// Failure to persist never changes the decision (advisory observability only).
func (d *decisionBridge) emitReceipt(r routeResult, method, tool string) {
	if !receiptWorthy(r) {
		return
	}
	_, _ = d.chain.appendReceipt(receipt{
		Event: "decision", Decision: r.Decision, Reason: r.Reason,
		Method: method, Tool: tool, Bucket: r.ConfBucket,
	})
}

// decideFull routes one request through the hub and also returns the raw SO
// answer, for callers (system1_decide) that need the label and confidence.
func (d *decisionBridge) decideFull(request []byte) (routeResult, soDecision) {
	r := d.decide(request)
	return r, r.LastDecision
}

// explicitCapability is the trust boundary: every opt-in must be present and
// true with a model-before phase. Absent fields never count as consent.
func explicitCapability(params []byte) bool {
	var m map[string]any
	if json.Unmarshal(params, &m) != nil {
		return false
	}
	for _, k := range []string{"need_label", "no_scratch", "safe", "reversible", "authority", "calibrated", "opt_in"} {
		if b, ok := m[k].(bool); !ok || !b {
			return false
		}
	}
	p, _ := m["phase"].(string)
	p = strings.ToLower(strings.ReplaceAll(p, "_", "-"))
	return p == "before-model" || p == "pre-model"
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

// enforcingGate inspects before forwarding. Write/diff guards and a spent
// session budget block; a missing router policy still fails open.
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
	if soMode() == soModeEnforcing && !explicitCapability(unwrapParams(request, f)) {
		routerAudit("gate", "reason", "cap-missing", "method", f.Method, "tool", tool)
		return rpcError(f.ID, -32001, "blocked: explicit decision capability required"), true
	}
	// Endpoint/key identity pinned at session start; drift fails closed only
	// in enforcing mode. Advisory mode never consults the pin.
	if soMode() == soModeEnforcing && string(soOracleIdentity()) != string(d.oraclePin) {
		routerAudit("gate", "reason", "so-unpinned-oracle", "method", f.Method)
		return rpcError(f.ID, -32001, "blocked: decision unavailable (so-unpinned-oracle)"), true
	}
	if f.Method == "tools/call" {
		if reason, blocked := d.guard.check(tool, toolArgs(f.Params)); blocked {
			routerAudit("writeguard", "tool", tool, "reason", reason)
			return rpcError(f.ID, -32001, "blocked: "+reason), true
		}
		args := toolArgs(f.Params)
		// The raw-args scan always runs, so metadata alongside a file body
		// (e.g. api_key next to content) cannot escape the diff-only path, and a
		// destructive shell command with no body is refused here.
		if v := argsGuardCheck(tool, args); v.Refuse {
			routerAudit("diffguard", "tool", tool, "reason", v.Code)
			return rpcError(f.ID, -32001, "blocked: "+v.Steer), true
		}
		if body, ok := writeContent(args); ok {
			p := argumentPath(args)
			// Security precheck over the text this call would add, before it lands.
			if v := diffSecurityCheck(p, oldContentOnDisk(p), body); v.Refuse {
				routerAudit("diffguard", "tool", tool, "reason", v.Code)
				return rpcError(f.ID, -32001, "blocked: "+v.Steer), true
			}
		}
	}
	allowed := routerAllow(tool)
	routerAudit("gate", "method", f.Method, "tool", tool,
		"allow", strconv.FormatBool(allowed))
	if soMode() == "enforcing" && !allowed {
		return rpcError(f.ID, -32001, "blocked: tool denied by router policy"), true
	}
	r := d.decideFrame(request)
	routerAudit("decision", "method", f.Method, "decision", r.Decision, "reason", r.Reason)
	// Enforcing only, and only when the decision is not an admit: an admitted
	// item's Steer is its own "continue" action, not a refusal to relay.
	if soMode() == "enforcing" && steerBlocks(r) {
		return rpcError(f.ID, -32001, withSteer("blocked", r.Steer)), true
	}
	// Spend only after the decision and any enforcing refusal: an admitted or
	// steered tool call is the budget boundary, not every policy check.
	if f.Method == "tools/call" && tool != "" {
		if _, max, spent := routerBudgetSpend(); !spent {
			routerAudit("budget", "tool", tool, "budget", "exhausted",
				"count", strconv.Itoa(max))
			return rpcError(f.ID, -32001, budgetSteer(max)), true
		}
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
	"so-low-conf": true, // low confidence is advice, never an enforcing block
	"ok":          true, "ledger-complete": true,
}

// advisoryHooks record router canary decisions after forwarding and mirror a
// scrubbed copy when a mirror is configured. The mirror response is ignored.
func (d *decisionBridge) advisoryHooks(request []byte) {
	f, err := decodeFrame(request)
	if err != nil {
		return
	}
	if !explicitCapability(unwrapParams(request, f)) {
		routerAudit("gate", "mode", soMode(), "reason", "cap-missing-warning", "method", f.Method)
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
		// Advisory cannot block, but it must audit the same secret shapes
		// enforcing would refuse, so the default mode stays observable.
		if v := argsGuardCheck(tool, toolArgs(f.Params)); v.Refuse {
			routerAudit("diffguard", "mode", "advisory", "tool", tool, "reason", v.Code)
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

// maxStdioFrame bounds one stdio frame so an unterminated or oversized frame
// cannot buffer without bound. It matches the HTTP body bound.
const maxStdioFrame = maxHTTPFrame

// readFrame reads one newline-terminated frame, capped at maxStdioFrame bytes.
// A frame past the cap is dropped with errFrameTooLarge rather than buffered.
func readFrame(br *bufio.Reader) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxStdioFrame {
			return nil, errFrameTooLarge
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}

// bridgeStdio is the fail-safe stdio interceptor. Advisory client frames are
// forwarded byte-for-byte before any decision work; enforcing frames are
// gated first. Initialize results get the MCP marker added.
func bridgeStdio(clientIn io.Reader, clientOut io.Writer, upstreamIn io.Writer, upstreamOut io.Reader) {
	d := newDecisionBridge()
	enforcing := soMode() == "enforcing"
	// clientOut has two writers: the enforcing block reply (client-reader
	// goroutine) and the forwarded upstream frame (main goroutine). Interleaved
	// writes larger than PIPE_BUF would corrupt the JSON-RPC framing, so both
	// writes are serialized.
	var outMu sync.Mutex
	// Advisory decision work runs on one worker off the read loop, so a slow
	// SO call never stalls the frames behind it. Enforcing gating stays inline
	// because a refusal must be answered before the frame is forwarded.
	advCh := make(chan []byte, 64)
	advDone := make(chan struct{})
	if !enforcing {
		go func() {
			defer close(advDone)
			for frame := range advCh {
				d.inspectClient(frame)
				d.advisoryHooks(frame)
			}
		}()
	} else {
		close(advDone)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(clientIn)
		for {
			frame, err := readFrame(br)
			if len(frame) > 0 {
				relayLog("client", frame)
				if enforcing {
					if resp, blocked := d.enforcingGate(frame); blocked {
						outMu.Lock()
						_, _ = clientOut.Write(resp)
						outMu.Unlock()
						if err != nil {
							break
						}
						continue
					}
				}
				// Forward first in advisory: never hold a client frame hostage.
				_, _ = upstreamIn.Write(frame)
				if !enforcing {
					select {
					case advCh <- append([]byte(nil), frame...):
					default: // a full queue drops the advisory copy, never the frame
					}
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
		frame, err := readFrame(br)
		if len(frame) > 0 {
			relayLog("server", frame)
			out := d.inspectServer(frame)
			outMu.Lock()
			_, _ = clientOut.Write(out)
			outMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	<-done // don't return until client frames are fully forwarded
	close(advCh)
	<-advDone
}

func (d *decisionBridge) inspectClient(frame []byte) {
	_ = d.decideFrame(frame)
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
