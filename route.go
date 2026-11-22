package main

import (
	"context"
	"encoding/json"
	"strings"
)

// Typed decisions only. No prose ever enters routeResult.Decision.
const (
	decPass = "pass"
	decPin  = "pin"
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
}

// stateEnvelope is buildState's output. All fields are user-sourced;
// server-sourced systemPrompt/message never reach it.
type stateEnvelope struct {
	State      string `json:"state"`
	NeedLabel  bool   `json:"need_label"`
	NoScratch  bool   `json:"no_scratch"`
	Safe       bool   `json:"safe"`
	Reversible bool   `json:"reversible"`
	OptIn      bool   `json:"opt_in"`
	Phase      string `json:"phase"`
	Authority  bool   `json:"authority"`
	Calibrated bool   `json:"calibrated"`
	Item       string `json:"item"` // optional decision_items.json entry (e.g. "tools.pick")
}

// droppedKeys are normalized key names removed recursively, before assembly.
// Includes MRTR input_required server content (systemPrompt/message) and every
// raw secret/conf field. normKey is defined in cache.go.
var droppedKeys = map[string]bool{
	"systemprompt": true, "message": true, "messages": true,
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

// scrubText handles non-JSON user text: strip key=value secrets and Bearer
// tokens, never guess at structure.
func scrubText(s string) string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	skipNext := false
	for _, f := range fields {
		if skipNext {
			skipNext = false
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
		out = append(out, f)
	}
	return strings.Join(out, " ")
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
		Phase:      stringField(obj, "phase"),
		Authority:  boolField(obj, "authority", "decision_authority"),
		Calibrated: boolField(obj, "calibrated"),
	}
	for _, k := range []string{
		"state", "request", "item", "need_label", "needLabel", "no_scratch", "noScratch",
		"safe", "reversible", "opt_in", "enable", "capability", "capabilities",
		"experimental", "phase", "authority", "decision_authority", "calibrated",
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

// inScope is the honest coverage intersection: model-before and authoritative and
// opted-in and calibrated. Context-only and post-tool phases never enter payday.
func inScope(e stateEnvelope) bool {
	p := strings.ToLower(strings.ReplaceAll(e.Phase, "_", "-"))
	return (p == "before-model" || p == "pre-model") && e.Authority && e.OptIn && e.Calibrated
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
			reason = "-32602" // capability missing: JSON-RPC invalid params code
		}
		return passResult(reason, false)
	}
	f := layer0{NeedLabel: e.NeedLabel, NoScratch: e.NoScratch, Safe: e.Safe, Reversible: e.Reversible, CleanRename: !staleRename(e.State)}
	if !gate(f) {
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
		d = askSOQ(e.State, item.question()) // only the cleaned user state crosses this boundary
	} else {
		d = askSO(e.State)
	}
	switch {
	case d.TimedOut:
		return passResult(classify(0, context.DeadlineExceeded), true)
	case d.Choice == "":
		return passResult("so-degraded", true) // HTTP!=200, parse error or a label outside the item's set
	case !admits(d.Choice):
		// Outside the item's proceed list nothing is admitted. A registry item's
		// own action is the steer; the generic question's label rides through
		// unchanged (gate.go treats a non-yes label as "pass through", so a
		// relay-side deny here would invert the gate).
		if isItem {
			r := passResult("registry-nonproceed", true)
			r.Label, r.Steer = d.Choice, item.Action[d.Choice]
			return r
		}
		r := passResult("so-"+d.Choice, true)
		r.Label = d.Choice
		return r
	case d.Conf < thr:
		r := passResult("so-low-conf", true)
		r.Label = d.Choice
		if isItem {
			// The item's own next step beats the generic "name the evidence"
			// text: a below-cut label still knows which action it wanted.
			r.Steer = item.Action[d.Choice]
		}
		return r
	default:
		bucket := confBucket(d.Conf)
		if p != nil {
			p.markSuccess(p.current().Name)
		}
		r := routeResult{Decision: decPin, Reason: "so-admit", Covered: true, ConfBucket: bucket}
		if isItem {
			r.Label, r.Steer = d.Choice, item.Action[d.Choice]
		}
		return r
	}
}
