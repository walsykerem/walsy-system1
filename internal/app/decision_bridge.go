package app

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	mu               sync.Mutex
	cache            *shardedCache
	pin              *pin
	keySet           bool
	marker           sync.Once
	guard            *writeGuard
	preflight        *preflightCalibration
	chainMu          sync.Mutex // guards chain and chainBad
	chain            *receiptChain
	chainBad         bool // loadReceiptChain failed closed: never append to a broken ledger
	flight           *soFlightGroup
	oraclePin        []byte // RAM-only sha256(endpoint||x00||key), set at session start
	endpointSnapshot string // async receipts never read the mutable soEndpoint global

	holdMu     sync.Mutex
	holdGen    uint64
	holdUntil  time.Time
	holdStrong bool // only >0.8 arms hysteresis; a 0.80 boundary admit does not
}

func newDecisionBridge() *decisionBridge {
	keySet := soAPIKey() != "" // presence only, never logged or forwarded to client
	oraclePin := soOracleIdentity()
	list, err := loadRegistry()
	if err != nil {
		list = bakedRegistry()
	}
	chain := loadReceiptChain()
	chainBad := chain == nil // never substitute a fresh ledger for a broken one
	return &decisionBridge{cache: newShardedCache(), pin: newPin(list), keySet: keySet,
		guard: newWriteGuard(), preflight: newPreflightCalibration(), chain: chain, chainBad: chainBad,
		flight: newSOFlightGroup(), oraclePin: oraclePin, endpointSnapshot: soEndpoint}
}

// receiptChainWriteMu serialises the reload-and-append check so two bridges in
// one process cannot both start from the same on-disk head.
var receiptChainWriteMu sync.Mutex

// appendChainReceipt reloads the persisted tail before appending: the ledger is
// shared on disk across bridges, and a broken/unreadable chain fails closed.
func (d *decisionBridge) appendChainReceipt(rec receipt) (string, error) {
	receiptChainWriteMu.Lock()
	defer receiptChainWriteMu.Unlock()
	d.chainMu.Lock()
	defer d.chainMu.Unlock()
	if d.chainBad || d.chain == nil {
		return "", errReceiptUnsafeFile
	}
	disk := loadReceiptChain()
	if disk == nil {
		d.chainBad = true
		return "", errReceiptUnsafeFile
	}
	d.chain = disk
	return d.chain.appendReceipt(rec)
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
// shortcut: polite/filler words in the state text are folded, no embedding or
// paraphrase tier; upgrade only with a measured accuracy-vs-hit sweep.
func gateKeyBytes(request []byte, cap capabilityConfig) [32]byte {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(request))
	dec.UseNumber()
	if dec.Decode(&obj) != nil {
		obj = nil
	}
	if obj != nil {
		for _, k := range []string{"id", "jsonrpc"} {
			delete(obj, k)
		}
		foldKeyState(obj)
	}
	b, _ := json.Marshal(obj)
	h := sha256.New()
	_, _ = h.Write([]byte(canonicalState(string(b))))
	// B4: bind the surviving question set into the namespace. Requires stays
	// local-only (json:"-"); only its applicability-derived names are hashed.
	for _, name := range activeQuestionNames(string(b)) {
		writeField(h, name)
	}
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

// foldKeyState drops only exact polite/filler tokens from state text so a
// rephrased request reuses its cell. Meaning-bearing words are never dropped.
var keyStateFillers = map[string]bool{
	"please": true, "kindly": true, "just": true, "simply": true,
	"this": true, "that": true, "same": true, "the": true, "a": true, "an": true,
}

func foldKeyState(obj map[string]any) {
	if s, ok := obj["state"].(string); ok {
		words := strings.Fields(s)
		out := words[:0]
		for _, w := range words {
			if !keyStateFillers[w] {
				out = append(out, w)
			}
		}
		obj["state"] = strings.Join(out, " ")
	}
	if params, ok := obj["params"].(map[string]any); ok {
		foldKeyState(params)
	}
}

// gateKey is the string wrapper kept for tests and callers that compare hex.
func gateKey(request []byte, cap capabilityConfig) string {
	k := gateKeyBytes(request, cap)
	return hex.EncodeToString(k[:])
}

// activeQuestionNames returns the ids (plus Requires names) of the questions
// that still apply to this raw request state, in stable order.
func activeQuestionNames(state string) []string {
	var names []string
	for id, it := range loadDecisionItems() {
		q := it.question()
		if !questionApplicable(q, state) {
			continue
		}
		names = append(names, id+":"+strings.Join(q.Requires, ","))
	}
	sort.Strings(names)
	return names
}

// buildSOState prepares the envelope for SO. Consent is never invented: missing
// opt-ins stay absent so buildState reads them false. User prompts are untrusted
// data and stay out of the SO state entirely; they remain in the local UI.
// A null return means the SO-bound skeleton could not be built safely; the
// caller must fail open without a POST (a rune-capped state is never truncated).
// buildSOStateDetailed keeps the typed ErrStateTruncated from capSOState so the
// decide hub can reject an over-cap envelope before any SO POST.
func buildSOStateDetailed(request string) (string, error) {
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
		return "", nil
	}
	// Prompt bodies never enter this state: only their ids and SHA-256 go on
	// the wire, and the whole payload is rune-capped before any POST.
	return soStateFromEnvelope(e)
}

func buildSOState(request string) string {
	state, _ := buildSOStateDetailed(request)
	return state
}

// terminalNegative are the reason-coded abstains safe to cache briefly. All
// transient/degraded/no-key/timeout states are deliberately absent so they are
// retried, not frozen into a fail-open cell.
var terminalNegative = map[string]bool{
	"so-low-conf": true, "so-no": true, "so-other": true, "registry-nonproceed": true,
}

// layer0SkipReason mirrors route's deterministic layer-0 exits so the bridge
// can return them before route's ambiguous shadow probe reaches SO. Keeping the
// probe for real decisions preserves FA5 calibration.
func layer0SkipReason(state string) string {
	var e stateEnvelope
	if json.Unmarshal([]byte(state), &e) != nil {
		return "no-state"
	}
	if !inScope(e) {
		// The frame lacks the decision capability (opt_in): name the missing
		// capability, not the JSON-RPC error code it used to be reported as.
		if !e.OptIn {
			return "layer0-opt-in"
		}
		return "layer0-scope"
	}
	f := layer0{NeedLabel: e.NeedLabel, NoScratch: e.NoScratch, Safe: e.Safe,
		Reversible: e.Reversible, CleanRename: !staleRename(e.State)}
	if !gate(f) {
		return "layer0-gate"
	}
	if staticShortCircuit(e.State) {
		return "layer0-static"
	}
	return ""
}

func skipResult(reason string) routeResult {
	r := passResult(reason, false)
	if r.Steer == "" {
		r.Steer = SteerText(reason, r.Label)
	}
	return r
}

// cacheResult projects a terminal route result into the cell the bridge stores.
func cacheResult(r routeResult) cachedDecision {
	if r.Abstained {
		label := r.Label
		if label == "" {
			label = r.LastDecision.Choice
		}
		// A terminal abstain's typed answer still crosses the cell boundary as
		// a coarse bucket, so a hit can rebuild it for toolsDecide like an admit.
		return cachedDecision{Abstain: true, Reason: r.Reason, ConfBucket: confBucket(r.LastDecision.Conf), Label: label, Steer: r.Steer}
	}
	label := r.Label
	if label == "" {
		label = r.LastDecision.Choice
	}
	return cachedDecision{Choice: "yes", ConfBucket: r.ConfBucket, Reason: "margin=1", Label: label, Steer: r.Steer}
}

// bucketConf maps the stored allowlisted bucket back to its lower bound, so a
// cache hit can return a typed answer without keeping a raw confidence in RAM.
func bucketConf(bucket string) float64 {
	switch bucket {
	case "1.0":
		return 1
	case "0.9":
		return 0.9
	case "0.8":
		return 0.8
	}
	return 0
}

// cachedResult serves a stored LLM-backed label and steer without a second SO
// POST. Only the coarse bucket crosses the cell boundary; the typed decision is
// rebuilt from it so tools on a cache hit see the same shape as on a live call.
func cachedResult(c cachedDecision) routeResult {
	if c.Abstain {
		label := c.Label
		if label == "" {
			label = c.Choice
		}
		return routeResult{Decision: decPass, Reason: c.Reason, Abstained: true, ConfBucket: c.ConfBucket, Label: label, Steer: c.Steer,
			LastDecision: soDecision{Choice: label, Conf: bucketConf(c.ConfBucket)}}
	}
	label := c.Label
	if label == "" {
		label = c.Choice
	}
	return routeResult{Decision: decPin, Reason: "cache-hit", Covered: true,
		ConfBucket: c.ConfBucket, Label: label, Steer: c.Steer,
		LastDecision: soDecision{Choice: label, Conf: bucketConf(c.ConfBucket)}}
}

// passesKeyConfRule enforces the rule already embedded in the cache key:
// conf >= 0.8 and, when the endpoint supplies probabilities, margin >= 0.10.
func passesKeyConfRule(d soDecision) bool {
	if d.Conf < 0.8 {
		return false
	}
	return !d.MarginKnown || d.Margin >= 0.10
}

// cacheKey binds the request key to the engine generation and pinned oracle
// identity. A rotated or swapped-out engine never reads another engine's cell;
// generation is identity, not a TTL.
func (d *decisionBridge) cacheKey(request []byte, cap capabilityConfig, gen uint64) [32]byte {
	return d.cacheKeyFromBase(gateKeyBytes(request, cap), gen)
}

// cacheKeyFromBase binds an already-computed gate key to engine identity.
func (d *decisionBridge) cacheKeyFromBase(base [32]byte, gen uint64) [32]byte {
	h := sha256.New()
	_, _ = h.Write(base[:])
	_ = binary.Write(h, binary.BigEndian, gen)
	_, _ = h.Write(d.oraclePin)
	_, _ = h.Write([]byte(d.endpointSnapshot)) // a shared cache must not mix endpoints
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// decide runs buildState + route per request, keyed by request+capability.
// No one-shot latch: a second distinct frame gets a fresh decision.
func (d *decisionBridge) decide(request []byte) routeResult {
	return d.decideWithKey(request, nil)
}

// decideWithKey reuses a gate key the caller already computed for this exact
// request; gateKeyBytes reads prompts from disk, so a frame-level caller must
// not pay for it twice.
func (d *decisionBridge) decideWithKey(request []byte, base *[32]byte) routeResult {
	d.mu.Lock()
	keySet := d.keySet
	d.mu.Unlock()
	if keySet {
		d.marker.Do(func() {
			emitMetric(metric{Seq: 1, PID: os.Getpid(), Route: markerKey, Reason: "key_set"})
		})
	}
	if !keySet {
		// Advisory fails open; enforcing must not pass a frame it never judged.
		if soMode() == soModeEnforcing {
			recordDecisionReason("no-key")
			return routeResult{Decision: decDeny, Reason: "no-key", Covered: true, Abstained: true,
				Steer: withSteer("blocked", "no classifier credential in enforcing mode")}
		}
		return passResult("no-key", false)
	}
	cap := soCapability()
	state, stateErr := buildSOStateDetailed(string(request))
	if errors.Is(stateErr, ErrStateTruncated) {
		return passResult("input_truncated", false)
	}
	p := preflightFromState(state)
	if reason := layer0SkipReason(state); reason != "" {
		r := skipResult(reason)
		recordPreflight(p, r, d.preflight)
		routerAudit("gate", "reason", reason)
		// skip-audit is an allowlisted corpus token for a policy skip. A frame
		// that only lacks the decision capability is a pass-through, so it gets
		// the descriptive reason row alone: no corpus token, no skip receipt.
		policySkip := reason != "layer0-opt-in"
		if policySkip {
			routerAudit("gate", "reason", "skip-audit")
		}
		if strings.HasPrefix(reason, "layer0-") && policySkip {
			d.emitReceiptDetailed(r, "", "", receiptDetails{Event: "skip", Force: true})
		}
		return r
	}
	gen := d.pin.generation()
	d.syncHold(gen)
	var key [32]byte
	if base != nil {
		key = d.cacheKeyFromBase(*base, gen)
	} else {
		key = d.cacheKey(request, cap, gen)
	}
	if cached, hit := d.cache.lookupForDecision(key); hit {
		r := cachedResult(cached)
		recordPreflight(p, r, d.preflight)
		admitStats.record(r.Decision == decPin)
		return r
	}
	r := d.flight.do(key, func() routeResult { return route(state, d.pin) })
	// Shadow only: preflight NEVER changes r. Recorded after r is final.
	recordPreflight(p, r, d.preflight)
	// FA5: an ambiguous shadow verdict is still probed in advisory mode, but
	// the probe stays in the calibration ring and never changes admission.
	if p.Ambiguous && r.LastDecision.Choice == "" && soMode() != soModeEnforcing && state != "" {
		adv := askSO(state)
		if d.preflight != nil {
			d.preflight.add("ambiguous", "so-"+adv.Choice)
		}
	}
	admitStats.record(r.Decision == decPin)
	admitConf := r.LastDecision.Conf
	// FA8: once this bridge has admitted, a later below-cut re-vote on the same
	// generation keeps the admit. Rotation clears the hold via syncHold.
	// shortcut: only a >0.8 admit arms the hold; a 0.80 boundary admit stays
	// reversible so conf 0.79 on a new state abstains (FA4). Upgrade: per-item
	// hysteresis thresholds from the registry.
	if r.Reason == "so-low-conf" && r.LastDecision.Conf >= 0.75 && d.holdStrongFor(gen) {
		r = routeResult{Decision: decPin, Reason: "so-admit-hysteresis", Covered: true,
			ConfBucket: confBucket(0.8), LastDecision: r.LastDecision}
	}
	// FA11: reason-coded ranking trace only; no raw confidence or probabilities.
	if r.LastDecision.Choice != "" {
		t := r.LastDecision.Trace
		slog.Info("ranking_trace", "strategy", t.Strategy, "tier", t.Tier,
			"comparable", t.Comparable, "score_kind", t.ScoreKind,
			"decided_by", t.DecidedBy, "winner", t.Winner,
			"runner_up", t.RunnerUp, "reason", t.Reason)
	}
	// Terminal admits and short-lived terminal abstains are cached; transient
	// and degraded states never are. Re-key after route: a successful admit may
	// bump the pin generation while this call was in flight.
	finalGen := d.pin.generation()
	// A mid-flight engine swap invalidates the answer: neither the old nor the
	// new generation may inherit a decision computed by the swapped-out engine.
	// shortcut: only generation changes are guarded; a pin expiry alone is not a swap. Upgrade: track per-engine answer provenance.
	if finalGen != gen {
		return r
	}
	storeOK := r.Decision == decPin && passesKeyConfRule(r.LastDecision)
	if storeOK || (r.Abstained && terminalNegative[r.Reason]) {
		d.cache.store(key, cacheResult(r))
		if storeOK {
			d.setHold(finalGen, admitConf)
		}
	}
	return r
}

func (d *decisionBridge) syncHold(gen uint64) {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	if d.holdGen != gen {
		d.holdGen, d.holdUntil, d.holdStrong = gen, time.Time{}, false
	}
}

func (d *decisionBridge) holdActive() bool {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	return time.Now().Before(d.holdUntil)
}

// holdStrongFor reads the hold flag under holdMu and binds it to gen, so a
// syncHold that cleared the flag on a generation change cannot leak a stale
// strong hold into this generation.
func (d *decisionBridge) holdStrongFor(gen uint64) bool {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	return d.holdGen == gen && d.holdStrong && time.Now().Before(d.holdUntil)
}

func (d *decisionBridge) setHold(gen uint64, conf float64) {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	d.holdGen, d.holdUntil = gen, time.Now().Add(cacheTTLPositive)
	d.holdStrong = conf > 0.8
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
// The one-value form is kept for existing callers; decideFrameDetailed exposes
// the typed SO answer so no second SO POST is needed for latency accounting.
func (d *decisionBridge) decideFrame(raw []byte) routeResult {
	r, _ := d.decideFrameDetailed(raw)
	return r
}

func (d *decisionBridge) decideFrameDetailed(raw []byte) (routeResult, soDecision) {
	// YC8 trust boundary: refuse a frame above the transport budget before any parse or SO call.
	if len(raw) >= maxHTTPFrame {
		return passResult("frame-too-large", false), soDecision{}
	}
	f, err := decodeFrame(raw)
	if err != nil {
		return passResult("no-state", false), soDecision{}
	}
	m := strings.ToLower(f.Method)
	if m != "initialize" && m != "tools/call" {
		routerAudit("gate", "reason", "layer0-scope", "method", f.Method)
		return passResult("layer0-scope", false), soDecision{}
	}
	params := unwrapParams(raw, f)
	keyHash := gateKeyBytes(params, soCapability())
	r := d.decideWithKey(params, &keyHash)
	tool := toolName(f.Params)
	det := receiptDetails{
		Force:     true, // every in-scope frame is chained, including local passes
		GateKey:   hex.EncodeToString(keyHash[:]),
		Item:      safeReceiptField(tool),
		Phase:     safeReceiptField(jsonStringField(f.Params, "phase")),
		Choice:    safeReceiptField(r.LastDecision.Choice),
		AdmitDeny: "deny",
		Outcome:   safeReceiptField(r.Reason),
		LatencyMS: r.LastDecision.LatencyMS,
		FrameID:   safeReceiptField(receiptFrameID(f)),
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	if r.Decision == decPin {
		det.AdmitDeny = "admit"
	}
	// A degraded advisory exception must be on the ledger before the caller
	// reads it; every other receipt stays off the reply path.
	d.emitReceiptDetailed(r, f.Method, tool, det)
	return r, r.LastDecision
}

type receiptDetails struct {
	Event, GateKey, Item, Phase, Choice, AdmitDeny, Outcome, FrameID, TS string
	LatencyMS                                                            int64
	Force                                                                bool // outbound receipts are recorded even for local passes
}

func safeReceiptField(s string) string {
	if v, ok := routerSafeField(s); ok {
		return v
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

func jsonStringField(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func receiptFrameID(f frame) string {
	if len(f.ID) > 0 {
		var v any
		if json.Unmarshal(f.ID, &v) == nil {
			switch x := v.(type) {
			case float64:
				return strconv.FormatInt(int64(x), 10)
			case string:
				return x
			}
		}
	}
	h := sha256.Sum256(f.Raw)
	return hex.EncodeToString(h[:])[:16]
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
	d.emitReceiptDetailed(r, method, tool, receiptDetails{})
}

func (d *decisionBridge) emitReceiptDetailed(r routeResult, method, tool string, det receiptDetails) {
	d.chainMu.Lock()
	chainBad, chainNil := d.chainBad, d.chain == nil
	d.chainMu.Unlock()
	if chainBad || chainNil || (!det.Force && !receiptWorthy(r)) {
		return
	}
	if strings.EqualFold(strings.TrimSpace(readSOConfig()["skip_receipts"]), "true") {
		return
	}
	event := det.Event
	if event == "" {
		event = "decision"
	}
	rec := receipt{
		Event: event, Decision: r.Decision, Reason: r.Reason,
		Method: method, Tool: tool, Bucket: r.ConfBucket,
		Source: "so", GateKey: det.GateKey, Item: det.Item, Phase: det.Phase,
		Choice: det.Choice, AdmitDeny: det.AdmitDeny, Outcome: det.Outcome,
		LatencyMS: det.LatencyMS, FrameID: det.FrameID, TS: det.TS,
	}
	// N8.2: advisory degraded exceptions are signed and time-bounded. The
	// keyed identity is RAM-only; no endpoint or key text reaches the ledger.
	if r.Reason == "so-degraded" && soMode() != soModeEnforcing {
		rec.ExpiresAt = time.Now().Add(30 * time.Minute).Unix()
		rec.Signature = d.receiptSignature(rec)
	}
	if fp := healthFingerprintWithEndpoint(d.endpointSnapshot); fp != nil {
		rec.ItemsSHA256, _ = fp["items_sha256"].(string)
		rec.ReceiptSHA256, _ = fp["receipt_sha256"].(string)
		rec.BuildSHA, _ = fp["build_sha"].(string)
	}
	_, _ = d.appendChainReceipt(rec)
}

func (d *decisionBridge) receiptSignature(rec receipt) string {
	m := hmac.New(sha256.New, d.oraclePin)
	for _, part := range []string{rec.Event, rec.Decision, rec.Reason, rec.Method, rec.Tool,
		rec.Bucket, rec.FrameID, rec.TS, strconv.FormatInt(rec.ExpiresAt, 10)} {
		_, _ = m.Write([]byte(part))
		_, _ = m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
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
	// N24: phase is context, not a capability. A phase-less frame must not be
	// refused in enforcing mode solely because the MCP client omitted it.
	return true
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
	// JSON-RPC 2.0 forbids replying to a notification (no id): gate it silently.
	block := func(msg string) ([]byte, bool) {
		if len(f.ID) == 0 {
			return nil, true
		}
		return rpcError(f.ID, -32001, msg), true
	}
	if soMode() == soModeEnforcing && !explicitCapability(unwrapParams(request, f)) {
		routerAudit("gate", "reason", "cap-missing", "method", f.Method, "tool", tool)
		return block("blocked: explicit decision capability required")
	}
	// E1: enforcement needs the promotion preconditions (M1 owner + SO-go >= 0.8)
	// before it can turn a decision into a refusal. gate.go:314-317 already
	// checks promotionOK before the POST; see INT-DB.md for the sim conflict
	// that prevents a second, earlier guard here.
	// Endpoint/key identity pinned at session start; drift fails closed only
	// in enforcing mode. Advisory mode never consults the pin.
	if soMode() == soModeEnforcing && string(soOracleIdentity()) != string(d.oraclePin) {
		routerAudit("gate", "reason", "so-unpinned-oracle", "method", f.Method)
		return block("blocked: decision unavailable (so-unpinned-oracle)")
	}
	if f.Method == "tools/call" {
		if reason, blocked := d.guard.check(tool, toolArgs(f.Params)); blocked {
			routerAudit("writeguard", "tool", tool, "reason", reason)
			return block("blocked: " + reason)
		}
		args := toolArgs(f.Params)
		// The raw-args scan always runs, so metadata alongside a file body
		// (e.g. api_key next to content) cannot escape the diff-only path, and a
		// destructive shell command with no body is refused here.
		if v := argsGuardCheck(tool, args); v.Refuse {
			routerAudit("diffguard", "tool", tool, "reason", v.Code)
			return block("blocked: " + v.Steer)
		}
		if body, ok := writeContent(args); ok {
			p := argumentPath(args)
			// Security precheck over the text this call would add, before it lands.
			if v := diffSecurityCheck(p, oldContentOnDisk(p), body); v.Refuse {
				routerAudit("diffguard", "tool", tool, "reason", v.Code)
				return block("blocked: " + v.Steer)
			}
		}
	}
	allowed := routerAllow(tool)
	routerAudit("gate", "method", f.Method, "tool", tool,
		"allow", strconv.FormatBool(allowed))
	if soMode() == "enforcing" && !allowed {
		return block("blocked: tool denied by router policy")
	}
	// Single latency charge lives in gate.go:336 routerBudgetSpendLatency.
	beforeCalls, _ := logsView()
	r, _ := d.decideFrameDetailed(request)
	routerAudit("decision", "method", f.Method, "decision", r.Decision, "reason", r.Reason)
	// GOV-1: a degraded SO response in enforcing mode is a reason-coded abstain;
	// a malformed HTTP 200 answer (bad_json) still keeps its historic fail-open path.
	afterCalls, logEntries := logsView()
	soOutage := afterCalls.Calls > beforeCalls.Calls && len(logEntries) > 0 &&
		logEntries[0].Kind == "systemone" &&
		(logEntries[0].Outcome == "so-degraded" || strings.HasPrefix(logEntries[0].Outcome, "http_"))
	if soMode() == soModeEnforcing && r.Decision == decDeny && r.Reason == "so-degraded" && soOutage {
		return block("blocked: " + r.Reason)
	}
	// Enforcing only, and only when the decision is not an admit: an admitted
	// item's Steer is its own "continue" action, not a refusal to relay.
	if soMode() == "enforcing" && steerBlocks(r) {
		return block(withSteer("blocked", r.Steer))
	}
	// Spend only after the decision and any enforcing refusal: an admitted or
	// steered tool call is the budget boundary, not every policy check.
	if f.Method == "tools/call" && tool != "" {
		if _, max, spent := routerBudgetSpend(); !spent {
			routerAudit("budget", "tool", tool, "budget", "exhausted",
				"count", strconv.Itoa(max))
			return block(budgetSteer(max))
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
	// SO outage is advice, never an enforcing verdict: the relay continues.
	"so-degraded": true, "invalid-response": true,
	"transient": true, "permanent": true, "unknown": true,
	"no-key": true, "no-state": true, "layer0-opt-in": true,
	"layer0-scope": true, "layer0-gate": true, "layer0-static": true,
	"layer0-shape": true, "registry-local": true,
	"so-admit": true, "so-no": true, "so-other": true, "cache-hit": true,
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

// scrubStrings applies scrubText to every surviving string leaf. cleanUser
// removes secret-KEYed fields first; free-text values can still carry Bearer
// tokens or key=value secrets, so scrub them before the mirror sees them.
func scrubStrings(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = scrubStrings(val)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = scrubStrings(e)
		}
		return x
	case string:
		return scrubText(x)
	default:
		return v
	}
}

func scrubbedMirror(request []byte) []byte {
	var obj any
	if json.Unmarshal(request, &obj) != nil {
		return []byte(`{}`)
	}
	b, _ := json.Marshal(scrubStrings(cleanUser(obj)))
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

// idleReadDeadline reports the per-frame read deadline for r. A stdio session
// passes *os.File, where minutes of idle are normal and EOF is the termination
// signal, so it gets no deadline: a timed-out read there leaves every later
// upstream frame unread. Sockets keep the deadline because the unix transport
// caps concurrent opens at 64 slots.
func idleReadDeadline(r io.Reader) (time.Time, bool) {
	if _, ok := r.(*os.File); ok {
		return time.Time{}, false
	}
	if _, ok := r.(interface{ SetDeadline(time.Time) error }); !ok {
		return time.Time{}, false
	}
	return time.Now().Add(httpReadTimeout), true
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
			// Refresh per frame so a long-lived socket session with traffic is
			// never cut at t+30s; stdio gets no deadline at all (long idle).
			if c, ok := clientIn.(interface{ SetDeadline(time.Time) error }); ok {
				if deadline, arm := idleReadDeadline(clientIn); arm {
					_ = c.SetDeadline(deadline)
				}
			}
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
		// Mirror the client read loop: a silent socket must not hold one of the
		// 64 unix-transport slots forever; a stdio session stays exempt.
		if c, ok := upstreamOut.(interface{ SetDeadline(time.Time) error }); ok {
			if deadline, arm := idleReadDeadline(upstreamOut); arm {
				_ = c.SetDeadline(deadline)
			}
		}
		frame, err := readFrame(br)
		if len(frame) > 0 {
			relayLog("server", frame)
			out := d.inspectServer(frame)
			d.inspectServerResponse(out, enforcing)
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

// inspectServerResponse is the outbound (server -> client) decision point.
// It never delays the reply: the gate result is chained synchronously and a
// failure to persist cannot rewrite the frame.
func (d *decisionBridge) inspectServerResponse(frame []byte, enforcing bool) {
	r := passResult("no-state", false)
	if enforcing {
		r, _ = d.decideFrameDetailed(frame)
	}
	f, _ := decodeFrame(frame)
	det := receiptDetails{Event: "outbound_response", Force: true,
		FrameID: safeReceiptField(receiptFrameID(f)),
		TS:      time.Now().UTC().Format(time.RFC3339)}
	d.emitReceiptDetailed(r, "response", "", det)
}

func (d *decisionBridge) inspectServer(frame []byte) []byte {
	f, err := decodeFrame(frame)
	if err != nil || !isInitializeResult(f) {
		return frame
	}
	// addRouteCapability rewrites the result payload. Round-trip the envelope
	// through encoding/json in the changed case: whitespace around the
	// "result" key must not defeat the splice. Other cases keep input bytes.
	newResult := addRouteCapability(f.Result)
	if bytes.Equal(newResult, f.Result) {
		return frame
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(frame, &env); err != nil {
		return frame
	}
	env["result"] = newResult
	out, err := json.Marshal(env)
	if err != nil {
		return frame
	}
	// readFrame keeps the trailing whitespace run; bridgeStdio writes replies
	// straight to clientOut, so dropping it would concatenate JSON-RPC frames.
	trimmed := bytes.TrimRight(frame, " \t\r\n")
	out = append(out, frame[len(trimmed):]...)
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
