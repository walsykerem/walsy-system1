package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fixed SO contract: one endpoint, one bounded shot, no retry. The bound is
// advisory only - client frames are flushed before this call.
//
// Evren is only one platform that speaks this contract, not a special case:
// any registry slot whose base matches soEndpoint's host supplies the key via
// soAPIKey, and WALSY_SO_ENDPOINT points the whole gate at another platform
// with no code change. Nothing below may branch on the platform name.
var (
	soEndpoint = soEndpointDefault()

	soEndpointDefaultBase = "https://evren-llmapi.ssyz.org.tr/v1/systemone"
	soTimeout             = 150 * time.Millisecond
	soEnforcingTimeout    = 5 * time.Second
	soClient              = &http.Client{}
)

// soOracleIdentity is a RAM-only hash of the SO endpoint + key identity with
// x00 separation. Key bytes are never stored or printed. A bridge pins this at
// session start; enforcing mode abstains if the endpoint/key drifts later.
func soOracleIdentity() []byte {
	h := sha256.New()
	_, _ = h.Write([]byte(soEndpoint))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(soAPIKey()))
	return h.Sum(nil)
}

// soEndpointDefault resolves the gate endpoint. Enforcing accepts only the
// baked base; advisory keeps the legacy override. Unsafe URLs (remote plaintext
// HTTP, embedded credentials) never win: they would leak the key.
func soEndpointDefault() string {
	v := os.Getenv("WALSY_SO_ENDPOINT")
	if v == "" {
		return soEndpointDefaultBase
	}
	if baseRejectReason(v) != "" {
		return soEndpointDefaultBase
	}
	if soMode() == soModeEnforcing && !sameSOEndpoint(v, soEndpointDefaultBase) {
		return soEndpointDefaultBase
	}
	return v
}

// sameSOEndpoint compares scheme, host, port and path without case noise.
func sameSOEndpoint(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return strings.EqualFold(a, b)
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		ua.Port() == ub.Port() && ua.Path == ub.Path
}

// FA9: cheapest-known SO endpoint first. Candidate list is opt-in via
// WALSY_SO_ENDPOINTS; without it the single configured endpoint is used, so
// existing pinning behaviour is unchanged. Latencies feed a per-URL EWMA.
var (
	soEndpointMu      sync.Mutex
	soEndpointLatency = map[string]struct {
		ewma float64
		n    int
	}{}
)

func soEndpointCandidates() []string {
	raw := os.Getenv("WALSY_SO_ENDPOINTS")
	if raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range strings.Split(raw, ",") {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] || baseRejectReason(v) != "" {
			continue
		}
		if soMode() == soModeEnforcing && !sameSOEndpoint(v, soEndpointDefaultBase) {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func soSelectEndpoint() string {
	cands := soEndpointCandidates()
	if len(cands) == 0 {
		return soEndpoint
	}
	soEndpointMu.Lock()
	defer soEndpointMu.Unlock()
	best := cands[0]
	for _, u := range cands[1:] {
		b, c := soEndpointLatency[best], soEndpointLatency[u]
		// Measured endpoints win; ties keep the first listed candidate.
		switch {
		case b.n == 0 && c.n > 0:
			best = u
		case b.n > 0 && c.n > 0 && c.ewma < b.ewma:
			best = u
		}
	}
	return best
}

func soRecordEndpoint(url string, d time.Duration) {
	if os.Getenv("WALSY_SO_ENDPOINTS") == "" {
		return
	}
	soEndpointMu.Lock()
	defer soEndpointMu.Unlock()
	s := soEndpointLatency[url]
	if s.n == 0 {
		s.ewma = float64(d)
	} else {
		s.ewma = 0.7*s.ewma + 0.3*float64(d)
	}
	s.n++
	soEndpointLatency[url] = s
}

// layer0 is the deterministic pre-gate. Raw prompts/headers never enter it.
type layer0 struct {
	NeedLabel   bool
	NoScratch   bool
	Safe        bool
	Reversible  bool
	CleanRename bool
}

// gate admits only the five-way conjunction; anything else passes through.
func gate(f layer0) bool {
	return f.NeedLabel && f.NoScratch && f.Safe && f.Reversible && f.CleanRename
}

// declLine matches a declaration being removed by a diff.
var declLine = regexp.MustCompile(`(?m)^-[ \t]*(?:func|type|var|const)[ \t]+(?:\([^)]*\)[ \t]*)?([A-Za-z_]\w*)`)

// staleRename reports a diff that deletes a declared symbol without a CALLER
// SCAN section: the model sees the hunk, never the references outside it.
func staleRename(state string) bool {
	if declLine.FindStringSubmatch(state) == nil {
		return false
	}
	return !strings.Contains(state, "CALLER SCAN")
}

// soDecision is the typed label. Conf drives admission only - never logged raw.
type soDecision struct {
	Choice        string // "yes" / "no" / "other"
	Conf          float64
	TimedOut      bool
	Margin        float64 // p1-p2; Only used when the endpoint supplies probabilities.
	MarginKnown   bool
	AnswerType    string
	Score         float64
	Probabilities map[string]float64
	LatencyMS     int64
	ResponseID    string       // "none" - no endpoint request-id exists. Never fabricated.
	Trace         RankingTrace // separate decision record; never an audit/metric field
	Reason        string       // typed failure reason (malformed_response / invalid_probabilities)
}

type soQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria,omitempty"`
	Legend       []string          `json:"legend,omitempty"`
	// Requires is a local capability check only. Never on the SO wire: no
	// shipped producer emits untrusted_text/tool_output into SO state today.
	Requires []string `json:"-"`
}

// questionApplicable is a local capability check: every Requires name must be
// a key in the state JSON object. It never travels on the SO wire.
func questionApplicable(q soQuestion, state string) bool {
	if len(q.Requires) == 0 {
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(state), &obj); err != nil || obj == nil {
		return false
	}
	for _, name := range q.Requires {
		if _, ok := obj[name]; !ok {
			return false
		}
	}
	return true
}

// promotionPreconditionsMet is the E1 gate: a named M1 owner and SO-go
// confidence >= 0.8. promotionOK runs it before the single audit probe so the
// refusal is fail-closed and the probe carries the precondition questions.
func promotionPreconditionsMet() bool {
	cfg := readSOConfig()
	if strings.TrimSpace(cfg["m1_approval_owner"]) == "" {
		return false
	}
	var conf float64
	if _, err := fmt.Sscanf(cfg["so_go_confidence"], "%f", &conf); err != nil {
		return false
	}
	return conf >= 0.8
}

// promotionOK keeps the fail-closed promotion check. When it denies an
// enforcing request, the single bounded SO probe still carries the missing
// preconditions as questions, so the refusal has an auditable payload.
func promotionOK() bool {
	if promotionPreconditionsMet() {
		return true
	}
	if soMode() == soModeEnforcing {
		_ = askSOQ(`{"state":"W24 approval promotion preconditions","need_label":true}`,
			soQuestion{Type: "choice", Criteria: map[string]string{
				"inside": "preconditions met", "outside": "preconditions missing",
			}})
	}
	return false
}

// soAPIKey resolves the SO provider key generically: a stored key on the
// provider whose base matches soEndpoint, then that provider's env var. The
// legacy EVREN_API_KEY env path stays for existing callers. Values never leave
// this process except as the x-api-key header.
func soAPIKey() string {
	if k := os.Getenv("EVREN_API_KEY"); k != "" {
		// M1: a bare env assignment has no named approver. Record the gap;
		// router_policy.go must allow the "approver" key to persist the name.
		approver := strings.TrimSpace(os.Getenv("WALSY_SO_APPROVER"))
		if approver == "" {
			approver = strings.TrimSpace(readSOConfig()["approver"])
		}
		if approver == "" {
			routerAudit("gate", "reason", "key-without-approver", "status", "unattended")
		} else {
			routerAudit("gate", "reason", "key-approved", "status", "approved")
		}
		return k
	}
	host := ""
	if u, err := url.Parse(soEndpoint); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		return ""
	}
	list, err := loadRegistry()
	if err != nil {
		list = bakedRegistry()
	}
	for _, pr := range list {
		if u, err := url.Parse(pr.Base); err != nil || !strings.EqualFold(u.Hostname(), host) {
			continue
		}
		if keys, err := loadKeys(pr.Name); err == nil {
			for _, k := range keys {
				if k != "" {
					return k
				}
			}
		}
		if pr.KeyEnv != "" {
			if k := os.Getenv(pr.KeyEnv); k != "" {
				return k
			}
		}
	}
	return ""
}

// askSO sends only the already secret-cleaned user state. Server-sourced
// systemPrompt/message must be stripped by the caller before this point.
// Any failure is a degraded passthrough: zero decision, never a 503.
func askSO(state string) soDecision {
	// A caller that did not request a label must not trigger a label probe:
	// preflight stays audit-only (need_label=false).
	var e struct {
		NeedLabel *bool `json:"need_label"`
	}
	if json.Unmarshal([]byte(state), &e) == nil && e.NeedLabel != nil && !*e.NeedLabel {
		return soDecision{}
	}
	return askSOQ(state, soQuestion{Type: "choice", Criteria: map[string]string{
		"yes":   "admit to the typed decision layer",
		"no":    "pass through unchanged",
		"other": "ambiguous; pass through",
	}})
}

// gateQuestion defaults to the generic yes/no/other question so askSO keeps its
// exact behaviour; a registry item brings its own instructions and labels.
func gateQuestion(q soQuestion) soQuestion {
	if len(q.Criteria) > 0 {
		return q
	}
	return soQuestion{Type: "choice", Criteria: map[string]string{
		"yes":   "admit to the typed decision layer",
		"no":    "pass through unchanged",
		"other": "ambiguous; pass through",
	}}
}

// contextQuestions adds the promotion/final probes only when the state asks
// for them, so ordinary frames keep the one-question SO wire contract.
func contextQuestions(state string) map[string]soQuestion {
	q := map[string]soQuestion{}
	lower := strings.ToLower(state)
	if strings.Contains(lower, "approval") || strings.Contains(lower, "m1") {
		q["go"] = soQuestion{Type: "choice", Criteria: map[string]string{
			"go":    "SO go confidence is at least 0.8",
			"no-go": "SO go confidence is below 0.8",
		}}
		q["m1_approval"] = soQuestion{Type: "choice", Criteria: map[string]string{
			"approved":     "a named M1 approval owner is recorded",
			"not-approved": "no named M1 approval owner",
		}}
	}
	if strings.Contains(lower, "final") || strings.Contains(lower, "complete") {
		q["final_complete"] = soQuestion{Type: "choice", Criteria: map[string]string{
			"complete":   "SO final complete confidence is at least 0.8",
			"incomplete": "SO final complete confidence is below 0.8",
		}}
	}
	return q
}

// askSOQ is the single SO POST, called only by route() inside the decide hub.
// A registry item brings its own instructions and labels, so the answer must
// be one of them.
// A below-cut answer is kept, not discarded: the label and confidence come back
// so the caller can steer with them. The cut is applied by the caller (route's
// so-low-conf branch, toolsDecide's !admitted branch), never by this parser.
func askSOQ(state string, q soQuestion) soDecision {
	// One guard: an open breaker or empty SO budget fails open with a zero
	// decision, before key resolution or any POST. E1 promotion preconditions
	// gate the promoting entry point (transport/bridge), never this gate POST.
	// Concurrency is capped at 2; excess POSTs are rejected immediately.
	if routerBreakerOpen("systemone") || routerBudgetLeft() <= 0 {
		return soDecision{}
	}
	if !questionApplicable(q, state) {
		routerAudit("gate", "reason", "no-applicable-question", "status", "skipped")
		return soDecision{}
	}
	select {
	case soSem <- struct{}{}:
		defer func() { <-soSem }()
	default:
		routerAudit("gate", "reason", "parallel-post-reject", "status", "rejected")
		return soDecision{}
	}
	start := time.Now()
	d := askSOQOnce(state, q)
	// T1/N28/FA10: SO attempts count in a bounded counter of their own; the
	// tool-call retry budget is spent only after admission by the bridge.
	n := soAttemptRecord()
	routerAudit("gate", "reason", "so-attempt", "count", fmt.Sprintf("%d", n))
	if d.TimedOut {
		if _, _, spent := routerBudgetSpendLatency(time.Duration(d.LatencyMS) * time.Millisecond); spent {
			routerAudit("gate", "reason", "so-latency-budget-spend", "status", "spent")
		}
	}
	// Typed parser rejections are dropped answers, not SO health failures;
	// transport errors and timeouts still open the breaker.
	shapeOK := d.Choice != "" || d.Reason == "invalid-response" || d.Reason == "invalid_probabilities"
	routerRecord("systemone", shapeOK && !d.TimedOut, time.Since(start))
	return d
}

// soAttempts counts SO POST attempts in this process, separate from the
// tool-call retry budget. It saturates so a long session cannot overflow it.
const soAttemptMax = 1 << 20

var (
	soAttemptMu sync.Mutex
	soAttemptN  int
)

func soAttemptRecord() int {
	soAttemptMu.Lock()
	defer soAttemptMu.Unlock()
	if soAttemptN < soAttemptMax {
		soAttemptN++
	}
	return soAttemptN
}

// soSem is the concurrency cap on live SO calls; a full semaphore fails open.
var soSem = make(chan struct{}, 2)

// routerBudgetLeft reads the shared router budget under routerMu. It lives here
// so askSOQ needs no new mutex and gate.go owns no router policy state.
func routerBudgetLeft() int {
	routerMu.Lock()
	defer routerMu.Unlock()
	routerInitLocked()
	return routerState.budgetLeft
}

// soCandidate is one provider in the decision pool. Every candidate carries
// its own credential: a key never travels to another provider's base.
type soCandidate struct {
	Name  string
	Base  string
	Key   string
	Model string
}

func bakedProviderBase(base string) bool {
	for i := range bakedProviders {
		if bakedProviders[i].Base == base {
			return true
		}
	}
	return false
}

// bakedEquivalentProvider keeps a baked loopback slot usable after a local port
// change; a remote or renamed slot still needs an exact baked base.
func bakedEquivalentProvider(p provider) bool {
	if bakedProviderBase(p.Base) {
		return true
	}
	for i := range bakedProviders {
		if strings.EqualFold(bakedProviders[i].Name, p.Name) && bakedEquivalentLoopback(bakedProviders[i], p) {
			return true
		}
	}
	return false
}

// soPool returns the operator's combo first, then the pinned System One
// endpoint as the backstop. The pool only grows on the default endpoint: an
// explicit override pins one platform and keeps stubbed tests hermetic.
func soPool() []soCandidate {
	if soEndpoint != soEndpointDefaultBase {
		// FA9: an explicit override still honours the latency-aware endpoint list.
		return []soCandidate{{Name: "systemone", Base: soSelectEndpoint(), Key: soAPIKey(), Model: soProbeModel}}
	}
	out := make([]soCandidate, 0, 4)
	steps, serr := loadCombo()
	providers, _, perr := loadRegistryWithOverrides()
	if serr == nil && perr == nil {
		enforcing := soMode() == soModeEnforcing
		seen := make(map[string]bool)
		for _, s := range steps {
			var p *provider
			for i := range providers {
				if providers[i].Name == s.Provider {
					p = &providers[i]
					break
				}
			}
			if p == nil || p.Base == "" || seen[p.Base] || baseRejectReason(p.Base) != "" {
				continue
			}
			// Enforcing pins the oracle: only baked-equivalent bases may carry a
			// key, so a registry edit cannot route it to an unvetted endpoint.
			if enforcing && !bakedEquivalentProvider(*p) {
				continue
			}
			key := ""
			if keys, _ := loadKeys(p.Name); len(keys) > 0 {
				key = keys[0]
			}
			if key == "" {
				key = os.Getenv(p.KeyEnv)
			}
			// J1: a loopback judge needs no key; a remote one without a key
			// must never enter the pool.
			if key == "" && !isLoopbackBase(p.Base) {
				continue
			}
			model := s.Model
			if model == "" {
				model = soProbeModel
			}
			seen[p.Base] = true
			out = append(out, soCandidate{Name: p.Name, Base: p.Base, Key: key, Model: model})
		}
	}
	back := soCandidate{Name: "systemone", Base: soEndpoint, Key: soAPIKey(), Model: soProbeModel}
	if back.Key != "" && baseRejectReason(back.Base) == "" {
		for _, c := range out {
			if c.Base == back.Base {
				return out
			}
		}
		out = append(out, back)
	}
	return out
}

// askSOQOnce walks the decision pool inside one bounded budget. Label
// validity is checked by the parser; the caller applies the confidence cut
// and all admission policy.
func askSOQOnce(state string, q soQuestion) soDecision {
	if !questionApplicable(q, state) {
		routerAudit("gate", "reason", "no-applicable-question", "status", "skipped")
		return soDecision{}
	}
	cands := soPool()
	if len(cands) == 0 {
		routerAudit("gate", "reason", "so-endpoint-rejected", "status", "denied")
		return soDecision{}
	}
	deadline := soTimeout
	if soMode() == soModeEnforcing {
		// T3: an enforcing request must not echo until the SO answers. The
		// advisory 150ms cap stays intact for FA9/N15 latency guards.
		deadline = soEnforcingTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	until := start.Add(deadline)
	questions := contextQuestions(state)
	questions["gate"] = gateQuestion(q)
	var reason string
	timedOut := false
	for i, cand := range cands {
		if ctx.Err() != nil {
			timedOut = true
			break
		}
		slice := time.Until(until) / time.Duration(len(cands)-i)
		if slice <= 0 {
			timedOut = true
			break
		}
		d, ok := soAskCandidate(ctx, cand, state, q, questions, slice)
		if ok {
			if cand.Name != "systemone" {
				routerAudit("gate", "provider", cand.Name)
			}
			return d
		}
		if d.Reason != "" && reason == "" {
			reason = d.Reason
		}
		if d.TimedOut {
			timedOut = true
		}
	}
	// A typed rejection from one candidate must not hide a timeout from another:
	// otherwise SO health and the latency budget read a dead oracle as healthy.
	if reason != "" || timedOut {
		return soDecision{Reason: reason, TimedOut: timedOut, LatencyMS: time.Since(start).Milliseconds()}
	}
	return soDecision{}
}

// soAskCandidate posts one gate question to one pool candidate. ok is true
// only for a usable SO answer; a transport or body failure fails over to the
// next candidate inside the shared deadline.
func soAskCandidate(ctx context.Context, cand soCandidate, state string, q soQuestion, questions map[string]soQuestion, slice time.Duration) (soDecision, bool) {
	cctx, cancel := context.WithTimeout(ctx, slice)
	defer cancel()
	// Stable identifiers are hashes only (never the raw prompt) so SO can cache
	// across frames without receiving prompt bodies. See N6.
	sid, pck := soStableIDs(cand.Model, q, state)
	// Live SO rejects top-level session_id/prompt_cache_key (HTTP 422). Keep
	// the stable hashes on the wire inside state so the cache contract holds.
	wireState := soWireState(state, sid, pck)
	body, err := json.Marshal(struct {
		Model     string                `json:"model"`
		State     string                `json:"state"`
		Questions map[string]soQuestion `json:"questions"` // object, never array
	}{
		Model:     cand.Model,
		State:     wireState,
		Questions: questions,
	})
	if err != nil {
		return soDecision{}, false
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, cand.Base, bytes.NewReader(body))
	if err != nil {
		return soDecision{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	// J1: a loopback judge needs no credential; keep the key on this machine.
	if !isLoopbackBase(cand.Base) {
		req.Header.Set("x-api-key", cand.Key)
	}

	start := time.Now()
	outcome, respB := "error", 0
	var usage map[string]any
	defer func() {
		latency := time.Since(start)
		soRecordEndpoint(cand.Base, latency)
		logCall("systemone", outcome, latency, len(body), respB, usage)
	}()

	resp, err := soClient.Do(req)
	if err != nil {
		d := soDecision{}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			outcome = "timeout"
			d.TimedOut = true
		}
		return d, false
	}
	defer resp.Body.Close()
	// Read one byte past the cap so an oversized body is rejected, never parsed
	// from a valid-looking prefix.
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	respB = len(raw)
	if len(raw) > 1<<20 {
		outcome = "malformed_response"
		routerAudit("gate", "reason", "malformed_response", "status", "oversized")
		return soDecision{Reason: "malformed_response"}, false
	}
	if resp.StatusCode != http.StatusOK {
		outcome = fmt.Sprintf("http_%d", resp.StatusCode)
		return soDecision{}, false
	}
	// F1: audit-only correlation. Only the request id is recorded;
	// x-flide-server-time-ms never reaches disk, a decision or a log.
	if id := resp.Header.Get("x-typesafe-request-id"); id != "" {
		routerAudit("gate", "response_id", id)
	}
	if rerr != nil {
		return soDecision{}, false
	}

	// Strict SO boundary: <=1 MiB (matching the read cap), exactly one JSON
	// document, no duplicate/case-variant keys, top-level allowlist only.
	// "provider" is a routing hint some platforms echo; it never steers.
	fields, err := strictObject(raw, 1<<20, map[string]bool{
		"model": true, "answers": true, "usage": true, "id": true,
		"session_id": true, "prompt_cache_key": true, // FA8: only if echoed
		"provider": true,
	})
	if err != nil {
		outcome = "bad_json"
		return soDecision{}, false
	}
	var out struct {
		Answers map[string]soAnswer `json:"answers"`
		Usage   map[string]any      `json:"usage"`
	}
	if b, ok := fields["answers"]; ok {
		out.Answers = map[string]soAnswer{}
		_ = json.Unmarshal(b, &out.Answers)
	}
	if b, ok := fields["usage"]; ok {
		_ = json.Unmarshal(b, &out.Usage)
		usage = out.Usage
	}
	if out.Answers == nil {
		outcome = "bad_json"
		return soDecision{}, false
	}
	a, ok := out.Answers["gate"]
	if !ok {
		outcome = "invalid-response"
		return soDecision{}, false
	}
	rawChoice := a.Choice
	a, reason, valid := validSOAnswer(a, q)
	if !valid {
		// N16: a generic answer naming no label in the registry is an unusable
		// response, not a typed item rejection. Real out-of-set registry labels
		// keep their precise invalid-response reason.
		if reason == "invalid-response" && isGenericQuestion(q) && !knownRegistryLabel(rawChoice) {
			outcome = "so-degraded"
			return soDecision{}, false
		}
		// YC5: a missing, misrouted or malformed answer is never a decision.
		outcome = reason
		routerAudit("gate", "reason", reason, "status", "rejected")
		return soDecision{Reason: reason}, false
	}
	d := soDecision{Choice: a.Choice, Conf: a.Confidence, AnswerType: a.Type,
		Score: a.Score, Probabilities: a.Probabilities,
		LatencyMS: time.Since(start).Milliseconds(), ResponseID: "none"}
	d.Trace = rankingTrace(d, "")
	if a.Noul {
		d.AnswerType = "noul" // explicit abstain, typed for FA11
		outcome = "ok"
		return d, true
	}
	if len(a.Probabilities) >= 2 {
		d.Margin, d.MarginKnown = answerMargin(a.Probabilities)
	}
	outcome = "ok"
	return d, true
}

// soAnswer is the whole decoded SO answer for one question. Strict decoding in
// askSOQOnce rejects duplicate/case-variant keys before this struct is filled;
// validSOAnswer then enforces the answer shape.
type soAnswer struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Type          string             `json:"type"`
	Score         float64            `json:"score"`
	Noul          bool               `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// isGenericQuestion identifies the router's yes/no/other gate question.
func isGenericQuestion(q soQuestion) bool {
	if len(q.Criteria) != 3 {
		return false
	}
	for _, label := range []string{"yes", "no", "other"} {
		if _, ok := q.Criteria[label]; !ok {
			return false
		}
	}
	return true
}

// knownRegistryLabel reports whether a label belongs to any registry item.
func knownRegistryLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, it := range loadDecisionItems() {
		if _, ok := it.Q.Labels[label]; ok {
			return true
		}
		if _, ok := it.Action[label]; ok {
			return true
		}
		for _, p := range it.Proceed {
			if p == label {
				return true
			}
		}
	}
	return false
}

// validSOAnswer requires a compatible answer type, a label from the question's
// own set, and a finite confidence in [0,1]. It normalizes the winner and the
// probability map. Returns the normalized answer, a typed failure reason
// (empty on success) and ok.
func validSOAnswer(a soAnswer, q soQuestion) (soAnswer, string, bool) {
	qt := strings.ToLower(strings.TrimSpace(q.Type))
	if qt == "" {
		qt = "choice"
	}
	at := strings.ToLower(strings.TrimSpace(a.Type))
	// FA11: score/choice are compatible only with a full distribution over the
	// question's labels. A bare score answer to a choice item is malformed (YC5).
	// "noul" is an explicit abstain for any question type.
	crossType := (at == "score" && qt == "choice") || (at == "choice" && qt == "score")
	fullDistribution := len(q.Criteria) > 0 && len(a.Probabilities) >= len(q.Criteria)
	if fullDistribution {
		for label := range q.Criteria {
			if _, ok := a.Probabilities[label]; !ok {
				fullDistribution = false
				break
			}
		}
	}
	compatible := at == qt || (crossType && fullDistribution)
	if at == "" || (!compatible && at != "noul") {
		return a, "invalid-response", false
	}
	a.Type = at
	a.Choice = strings.ToLower(strings.TrimSpace(a.Choice))
	if at == "noul" || a.Noul {
		return a, "", true
	}
	if a.Choice == "" {
		return a, "invalid-response", false
	}
	if len(q.Criteria) > 0 {
		if _, inSet := q.Criteria[a.Choice]; !inSet {
			// FA2.2: criteria are the authority; accept a case-variant key but
			// store the canonical label.
			found := ""
			for k := range q.Criteria {
				if strings.EqualFold(k, a.Choice) {
					found = strings.TrimSpace(k)
					break
				}
			}
			if found == "" {
				return a, "invalid-response", false
			}
			a.Choice = found
		}
	}
	if math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) || a.Confidence < 0 || a.Confidence > 1 {
		return a, "invalid-response", false
	}
	if len(a.Probabilities) > 0 {
		if len(a.Probabilities) < 2 {
			return a, "invalid_probabilities", false
		}
		sum := 0.0
		for label, v := range a.Probabilities {
			if strings.TrimSpace(label) == "" || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				return a, "invalid_probabilities", false
			}
			sum += v
		}
		// 1% tolerance plus float rounding slack: a decimal distribution
		// summing to 1.01 must not be rejected on binary representation noise.
		if math.Abs(sum-1) > 0.01+1e-9 {
			return a, "invalid_probabilities", false
		}
		// YD8: sum 0.99 is within tolerance; normalize before the margin cut.
		if sum > 0 {
			for k, v := range a.Probabilities {
				a.Probabilities[k] = v / sum
			}
		}
	}
	return a, "", true
}

// soWireState embeds the FA8 cache hashes without breaking states that are
// JSON envelopes: those gain one inert string field. Plain-text states keep
// the newline form. Live SO rejects the same keys at top level (HTTP 422).
func soWireState(state, sid, pck string) string {
	contract := fmt.Sprintf("so_session_id=%s\nso_prompt_cache_key=%s", sid, pck)
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(state), &obj) == nil && obj != nil {
		if v, err := json.Marshal(contract); err == nil {
			obj["so_cache_contract"] = v
			if out, err := json.Marshal(obj); err == nil {
				return string(out)
			}
		}
	}
	return state + "\n" + contract
}

// soStableIDs derives FA8 identifiers from the model, canonical question and a
// state hash. Never the raw prompt: N6 keeps prompt bodies off the wire.
func soStableIDs(model string, q soQuestion, state string) (string, string) {
	h := sha256.New()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	if qb, err := json.Marshal(q); err == nil {
		_, _ = h.Write(qb)
	}
	_, _ = h.Write([]byte{0})
	sh := sha256.Sum256([]byte(state))
	_, _ = h.Write(sh[:])
	id := fmt.Sprintf("%x", h.Sum(nil))
	return id, id
}

// answerMargin returns p1-p2 over a label probability map. Only the endpoint's
// own distribution counts; when absent the legacy confidence path is unchanged.
func answerMargin(p map[string]float64) (float64, bool) {
	if len(p) < 2 {
		return 0, false
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, second := 0.0, 0.0
	for _, k := range keys {
		v := p[k]
		if v > best {
			second, best = best, v
		} else if v > second {
			second = v
		}
	}
	return best - second, true
}
