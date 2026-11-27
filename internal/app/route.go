package app

import (
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

// Typed decisions only. No prose ever enters routeResult.Decision.
const (
	decPass = "pass"
	decPin  = "pin"
	decDeny = "deny"
)

// routeResult is the one routing output. Reason is a reason-code, never a
// prose explanation; ConfBucket is the coarse allowlisted bucket only.
type routeResult struct {
	Decision    string
	Reason      string
	Covered     bool
	Abstained   bool
	Passthrough bool
	ConfBucket  string
	// Label is the admitted registry label, when one is known. Without it an
	// admitted decision cannot say what it admitted.
	Label string
	// Steer is the next step to tell the agent, when one is known. Registry
	// items carry it per label; a refusal that says nothing is retried blind.
	Steer string
	// LastDecision is the raw SO answer behind the result, when SO was asked.
	// Bucket-only and layer-0 results leave it zero.
	LastDecision soDecision
}

// stateEnvelope is buildState's output. All fields are user-sourced;
// server-sourced systemPrompt/message never reach it.
type stateEnvelope struct {
	State      string `json:"state"`
	Method     string `json:"method"` // trusted JSON-RPC method from the envelope, not client params
	NeedLabel  bool   `json:"need_label"`
	NoScratch  bool   `json:"no_scratch"`
	Safe       bool   `json:"safe"`
	Reversible bool   `json:"reversible"`
	OptIn      bool   `json:"opt_in"`
	Phase      string `json:"phase"`
	Authority  bool   `json:"authority"`
	Calibrated bool   `json:"calibrated"`
	Item       string `json:"item"` // optional decision_items.json entry (e.g. "tools.pick")
	// UserSystemPrompt is untrusted data; the header marks the boundary for SO.
	UserSystemPrompt  string `json:"user_system_prompt,omitempty"`
	UserSystemPrompts string `json:"user_system_prompts,omitempty"`
	// Policy A decision shape: carried from the caller's request into the SO
	// skeleton. Omission/unverified are derived, never read from these fields.
	Request  string   `json:"request,omitempty"`
	Criteria []string `json:"criteria,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// soStateFromEnvelope projects an envelope into the explicit SO-bound shape.
// Only prompt IDs and SHA-256 digests cross; prompt bodies are never a field.
// Omission is mechanical: the audit of this very state names what was left
// undone (omitted) and which named risks stayed unverified. A caller cannot
// claim an empty omission list by omitting it from the request.
// Over-cap state is distilled to bounded verbatim Evidence with a mechanical
// omission marker; what cannot fit verbatim is rejected by marshalSOState.
func soStateFromEnvelope(e stateEnvelope) (string, error) {
	skel := StateSkeleton{
		State: e.State, Method: e.Method, NeedLabel: e.NeedLabel, NoScratch: e.NoScratch,
		Safe: e.Safe, Reversible: e.Reversible, OptIn: e.OptIn, Phase: e.Phase,
		Authority: e.Authority, Calibrated: e.Calibrated, Item: e.Item,
		Criteria: e.Criteria, Evidence: e.Evidence,
	}
	// buildState folds a lone "request" into state; do not pay for it twice.
	if e.Request != "" && e.Request != e.State {
		skel.Request = e.Request
	}
	// Only a JSON state has an auditable ledger. Prose/absent states have no
	// omission to declare, so they must not gain a synthetic "state unreadable".
	if strings.HasPrefix(strings.TrimSpace(e.State), "{") {
		for _, m := range finalAudit(e.State).Missing {
			if rest, ok := strings.CutPrefix(m, "risk: "); ok {
				skel.Unverified = append(skel.Unverified, strings.TrimSpace(rest))
				continue
			}
			skel.Omitted = append(skel.Omitted, m)
		}
	}
	bodies := map[string]string{}
	for _, p := range loadPrompts() {
		bodies[p.Name] = p.Text
	}
	ref, err := buildStateSkeleton("", bodies)
	if err != nil {
		return "", err
	}
	skel.Prompts = ref.Prompts
	if utf8.RuneCountInString(e.State) > maxSOStateChars {
		if d, ok := distillSOState(skel); ok {
			skel = d
		}
	}
	return marshalSOState(skel)
}

// droppedKeys are normalized key names removed recursively, before assembly.
// Includes MRTR input_required server content (systemPrompt/message) and every
// raw secret/conf field. normKey is defined in cache.go.
var droppedKeys = map[string]bool{
	"systemprompt": true, "usersystemprompt": true, "usersystemprompts": true, "message": true, "messages": true,
	"tooloutput": true, "toolresult": true, "toolcallid": true,
	"token": true, "tokens": true, "apikey": true, "authorization": true,
	"auth": true, "header": true, "headers": true, "env": true,
	"conf": true, "confidence": true, "cost": true, "key": true,
	"secret": true, "password": true, "email": true, "phone": true,
	"ssn": true, "pii": true,
}

func dropKey(k string) bool { return droppedKeys[normKey(k)] }

// cleanUser removes server-sourced and secret keys at every depth. It runs
// before any field is combined into the state envelope.
func cleanUser(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if dropKey(k) {
				continue
			}
			out[k] = cleanUser(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cleanUser(e)
		}
		return out
	default:
		return v
	}
}

// credentialWithheld is the only replacement written to outbound text; the
// raw shape never survives, and no secret is echoed.
const credentialWithheld = "[withheld: credential]"

// scrubText handles non-JSON user text: strip key=value secrets and Bearer
// tokens, then replace credential-shaped spans with the write gate's own
// matchers (diffSecretPatterns), never guess at structure.
func scrubText(s string) string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	skipNext := false
	for _, f := range fields {
		if skipNext {
			skipNext = false
			// A header value starting with a scheme word (Authorization: Bearer x)
			// hides the secret one token further along, so keep skipping.
			if strings.ToLower(strings.Trim(f, ":,;\"'")) == "bearer" {
				skipNext = true
			}
			continue
		}
		lower := strings.ToLower(strings.Trim(f, ":,;\"'"))
		if lower == "bearer" || strings.HasPrefix(lower, "bearer=") {
			skipNext = true
			continue
		}
		if i := strings.IndexByte(f, '='); i >= 0 && dropKey(f[:i]) {
			continue
		}
		if strings.HasSuffix(f, ":") && dropKey(strings.TrimSuffix(f, ":")) {
			skipNext = true
			continue
		}
		out = append(out, f)
	}
	joined := strings.Join(out, " ")
	if diffSecretKind(joined) == "" {
		return joined
	}
	for _, re := range diffSecretPatterns {
		joined = re.ReplaceAllString(joined, credentialWithheld)
	}
	return joined
}

func boolField(obj map[string]any, keys ...string) bool {
	for _, k := range keys {
		if b, ok := obj[k].(bool); ok {
			return b
		}
	}
	return false
}

func stringField(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok {
			return s
		}
	}
	return ""
}

// stringListField reads the first non-empty string array from obj. Only strings
// enter the shape; numbers/objects are dropped rather than stringified.
func stringListField(obj map[string]any, keys ...string) []string {
	for _, k := range keys {
		arr, ok := obj[k].([]any)
		if !ok {
			continue
		}
		var out []string
		for _, v := range arr {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// capabilityEnabled reads the opt-in marker from the user request only.
// SDK default is off: absent capability means no decision traffic change.
func capabilityEnabled(obj map[string]any) bool {
	if b, ok := obj["opt_in"].(bool); ok {
		return b
	}
	if b, ok := obj["enable"].(bool); ok {
		return b
	}
	for _, name := range []string{"capability", "capabilities", "experimental"} {
		cap, ok := obj[name].(map[string]any)
		if !ok {
			continue
		}
		m, ok := cap[markerKey].(map[string]any)
		if ok {
			if b, ok := m["enabled"].(bool); ok {
				return b
			}
		}
	}
	return false
}

// buildState produces the SO state string. Secret-clean runs first; only the
// user request is an input. Server-sourced systemPrompt/message are dropped.
func buildState(userRequest string) string {
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(userRequest))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		text := scrubText(userRequest)
		b, _ := json.Marshal(stateEnvelope{State: text, Item: inferItem("", "", "", text)})
		return string(b)
	}

	clean, _ := cleanUser(obj).(map[string]any)
	e := stateEnvelope{
		NeedLabel:  boolField(obj, "need_label", "needLabel"),
		NoScratch:  boolField(obj, "no_scratch", "noScratch"),
		Safe:       boolField(obj, "safe"),
		Reversible: boolField(obj, "reversible"),
		OptIn:      capabilityEnabled(obj),
		Phase:      strings.ToLower(strings.ReplaceAll(stringField(obj, "phase"), "_", "-")),
		Method:     stringField(obj, "method"),
		Authority:  boolField(obj, "authority", "decision_authority"),
		Calibrated: boolField(obj, "calibrated"),
		Request:    stringField(obj, "request"),
		Criteria:   stringListField(obj, "criteria"),
		Evidence:   stringListField(obj, "evidence"),
	}
	for _, k := range []string{
		"state", "request", "item", "need_label", "needLabel", "no_scratch", "noScratch",
		"safe", "reversible", "opt_in", "enable", "capability", "capabilities",
		"experimental", "phase", "authority", "decision_authority", "calibrated", "method",
	} {
		delete(clean, k)
	}
	if s, ok := obj["state"].(string); ok && s != "" {
		e.State = scrubText(s)
	} else if s, ok := obj["request"].(string); ok && s != "" {
		e.State = scrubText(s)
	} else {
		b, _ := json.Marshal(clean)
		e.State = string(b)
	}
	// User prompts are carried as data, never merged into the instruction state.
	if p, ok := obj["user_system_prompt"].(string); ok && p != "" {
		e.UserSystemPrompt = p
	}
	if p, ok := obj["user_system_prompts"].(string); ok && p != "" {
		e.UserSystemPrompts = p
	}

	// An explicit item id wins, honoured only when the registry knows it. With
	// no explicit id, inferItem may name one from the frame's own shape; an
	// unknown or ambiguous frame keeps the generic path ("").
	if id, _ := obj["item"].(string); id != "" {
		if _, known := decisionItemFor(id); known {
			e.Item = id
		}
	} else {
		e.Item = inferItem(stringField(obj, "method"),
			stringField(obj, "tool", "name"), e.Phase, e.State)
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func passResult(reason string, abstained bool) routeResult {
	return routeResult{Decision: decPass, Reason: reason, Abstained: abstained, Passthrough: true}
}

// enforcingUnavailable is GOV-1: a missing SO answer in enforcing mode is a
// reason-coded abstain, never a fail-open pass.
func enforcingUnavailable() routeResult {
	recordDecisionReason("so-degraded")
	return routeResult{Decision: decDeny, Reason: "so-degraded", Covered: true,
		Abstained: true, Steer: withSteer("blocked", "SO unavailable in enforcing mode")}
}

// invalidSOProbabilities rejects a distribution that cannot be a probability
// map: too few labels, a non-finite/out-of-range value, or a sum away from 1.
// An absent map stays on the legacy confidence path.
func invalidSOProbabilities(p map[string]float64) bool {
	if len(p) == 0 {
		return false
	}
	if len(p) < 2 {
		return true
	}
	sum := 0.0
	for _, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return true
		}
		sum += v
	}
	return math.Abs(sum-1) > 0.01
}

// inScope is the honest coverage intersection: an in-scope JSON-RPC method,
// authoritative, opted-in and calibrated. A missing phase or an explicit
// after-model/after-tool phase is past the decision point and stays out; an
// unknown phase remains in scope so unusual-but-declared callers keep working.
func inScope(e stateEnvelope) bool {
	m := strings.ToLower(strings.ReplaceAll(e.Method, "_", "-"))
	if m != "initialize" && m != "tools/call" {
		return false
	}
	ph := strings.ToLower(strings.ReplaceAll(e.Phase, "_", "-"))
	switch ph {
	case "", "after-model", "post-model", "after-tool", "post-tool",
		"after-call", "post-call", "after-response", "post-response":
		return false
	}
	return e.Authority && e.OptIn && e.Calibrated
}

// staticPass is the deterministic microsecond short-circuit.
var staticPass = map[string]bool{
	"ping": true, "pong": true, "help": true, "version": true,
	"status": true, "health": true, "list": true, "uptime": true,
}

func staticShortCircuit(text string) bool {
	return staticPass[strings.Join(strings.Fields(strings.ToLower(text)), " ")]
}

// isLabelShaped keeps AGENTS.md gate-shaped work only; everything else passes.
func isLabelShaped(text string) bool {
	t := strings.ToLower(text)
	for _, k := range []string{
		"classify", "label", "score", "rank", "route", "triage", "decide",
		"decision", "yes/no", "go/no", "sentiment", "intent", "moderat",
		"spam", "anomal", "risk", "routing", "gate", "should",
	} {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

// route runs Layer 0 -> Layer 1 -> Layer 2. Every failure is a transparent
// passthrough; no 503, no fail-closed, no raw conf/secret in the result.
// A non-admit answer always carries the one concrete next step, so a
// below-0.8 verdict steers instead of sending the caller back to rediscover it.
// ledgerMetadataMissing names the owner/round fields a done claim must carry.
// Who/When are shape-agnostic; a missing or empty value is not an owner.
func ledgerMetadataMissing(state string) []string {
	var s todoLedgerState
	if json.Unmarshal([]byte(state), &s) != nil {
		return []string{"who", "when"}
	}
	var missing []string
	for _, f := range []struct {
		name string
		raw  json.RawMessage
	}{{"who", s.Who}, {"when", s.When}} {
		if ledgerValueEmpty(f.raw) {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// ledgerValueEmpty treats a blank string or an empty container as no metadata.
// Other shapes (numbers, booleans) still count: Who/When are shape-agnostic.
func ledgerValueEmpty(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return true
	}
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

func route(state string, p *pin) routeResult {
	r := routeInner(state, p)
	if r.Steer == "" && r.Decision != decPin {
		r.Steer = SteerText(r.Reason, r.Label)
	}
	return r
}

func routeInner(state string, p *pin) routeResult {
	var e stateEnvelope
	if json.Unmarshal([]byte(state), &e) != nil {
		return passResult("no-state", false)
	}

	// Layer 0: deterministic, us - no SO call.
	if !inScope(e) {
		reason := "layer0-scope"
		if !e.OptIn {
			reason = "layer0-opt-in" // capability missing: no opt_in on the frame
		}
		return passResult(reason, false)
	}
	f := layer0{NeedLabel: e.NeedLabel, NoScratch: e.NoScratch, Safe: e.Safe, Reversible: e.Reversible, CleanRename: !staleRename(e.State)}
	if !gate(f) {
		// FA5: an ambiguous shadow verdict still reaches SO for calibration.
		// The admission outcome is unchanged: this stays a layer-0 pass.
		if p := preflightFromState(state); p.Ambiguous {
			_ = askSO(state)
		}
		return passResult("layer0-gate", false)
	}
	if staticShortCircuit(e.State) {
		return passResult("layer0-static", false)
	}
	// Layer 1: one SO shot.
	// A registry item brings its own question, label set and proceed list. An
	// explicit known item is already decision-shaped, so it skips layer0-shape.
	item, isItem := decisionItemFor(e.Item)
	// A per-item cut may only lower the global gate (clamp inside threshold).
	thr := defaultThreshold()
	if isItem {
		thr = item.threshold(thr)
	}
	if isItem && item.ID == "antishirk.todo_ledger" {
		// Deterministic: the declared todo list against the done list, no model call.
		l := todoLedgerFrom(e.State)
		switch l.Verdict.Choice {
		case "complete":
			// Risks named in the same state are part of the done claim.
			if a := finalAudit(e.State); !a.OK {
				r := passResult("ledger-open", true)
				r.Label = "do_more"
				r.Steer = a.Steer
				return r
			}
			if missing := ledgerMetadataMissing(e.State); len(missing) > 0 {
				r := passResult("ledger-open", true)
				r.Label = "do_more"
				r.Steer = "do not claim done: missing " + strings.Join(missing, ", ") + " (owner and round for every covered item)"
				return r
			}
			return routeResult{Decision: decPin, Reason: "ledger-complete", Covered: true, ConfBucket: confBucket(l.Verdict.Conf), Label: l.Verdict.Choice, Steer: item.Action[l.Verdict.Choice]}
		case "":
			return passResult("no-state", false)
		}
		r := passResult("ledger-open", true)
		r.Label = l.Verdict.Choice
		r.Steer = "finish or explicitly drop: " + strings.Join(l.Uncovered, "; ")
		return r
	}
	if !isItem && !isLabelShaped(e.State) {
		return passResult("layer0-shape", false)
	}
	if isItem && !item.Gate {
		return passResult("registry-local", false) // AGENTS.md:66 - the agent's own call, no model
	}
	admits := func(choice string) bool { return choice == "yes" }
	if isItem {
		admits = item.admits
	}

	var d soDecision
	if isItem {
		q := item.question()
		// YC7: a Requires gate is only checkable on a structured state. FA11
		// typed probes carry free-form state, so keep their wire question
		// unconditional; JSON states still suppress missing declared fields.
		if strings.HasPrefix(strings.TrimSpace(state), "{") {
			if !questionApplicable(q, state) {
				return passResult("no-applicable-question", false)
			}
		} else {
			q.Requires = nil
		}
		d = askSOQ(state, q)
	} else {
		d = askSO(state)
	}
	// LastDecision keeps labels/confidence out of logs and the cache; callers
	// that need the raw answer (system1_decide) read it from this result.
	raw := d
	withRaw := func(r routeResult) routeResult { r.LastDecision = raw; return r }
	switch {
	case d.Reason != "":
		// Typed gate failures (malformed_response, invalid_probabilities) are
		// terminal abstains with their own reason code, never so-degraded.
		return withRaw(passResult(d.Reason, true))
	case d.AnswerType == "noul":
		// FA11: an explicit no-answer is a terminal abstain, never a degraded pass.
		recordDecisionReason("so-noul")
		return withRaw(passResult("so-noul", true))
	case d.TimedOut:
		if soMode() == soModeEnforcing {
			return withRaw(enforcingUnavailable())
		}
		return withRaw(passResult("timeout", true))
	case d.Choice == "":
		if soMode() == soModeEnforcing {
			return withRaw(enforcingUnavailable())
		}
		recordDecisionReason("so-degraded")
		return withRaw(passResult("so-degraded", true)) // HTTP!=200, parse error or a label outside the item's set
	case invalidSOProbabilities(d.Probabilities):
		// W35: a broken distribution is an abstain, not an admit on confidence alone.
		return withRaw(passResult("invalid_probabilities", true))
	case !admits(d.Choice):
		// Outside the item's proceed list nothing is admitted. A registry item's
		// own action is the steer; the generic question's label rides through
		// unchanged (gate.go treats a non-yes label as "pass through", so a
		// relay-side deny here would invert the gate).
		if isItem {
			r := passResult("registry-nonproceed", true)
			r.Label, r.Steer = d.Choice, item.Action[d.Choice]
			return withRaw(r)
		}
		r := passResult("so-"+d.Choice, true)
		r.Label = d.Choice
		return withRaw(r)
	case d.Conf < thr:
		r := passResult("so-low-conf", true)
		r.Label = d.Choice
		if isItem {
			// The item's own next step beats the generic "name the evidence"
			// text: a below-cut label still knows which action it wanted.
			r.Steer = item.Action[d.Choice]
		}
		return withRaw(r)
	default:
		bucket := confBucket(d.Conf)
		if p != nil {
			p.markSuccess(p.current().Name)
		}
		r := routeResult{Decision: decPin, Reason: "so-admit", Covered: true, ConfBucket: bucket}
		if isItem {
			r.Label, r.Steer = d.Choice, item.Action[d.Choice]
		}
		return withRaw(r)
	}
}
