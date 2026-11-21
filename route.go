package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Typed decisions only. No prose ever enters routeResult.Decision.
const (
	decPass = "pass"
	decPin  = "pin"
	decDeny = "deny" // reserved for deterministic security/scope rejection
	decAsk  = "ask"  // reserved for explicit human/ask handoff
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
		b, _ := json.Marshal(stateEnvelope{State: scrubText(userRequest)})
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
		"state", "request", "need_label", "needLabel", "no_scratch", "noScratch",
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
	b, _ := json.Marshal(e)
	return string(b)
}

func passResult(reason string, abstained bool) routeResult {
	return routeResult{Decision: decPass, Reason: reason, Abstained: abstained, Passthrough: true}
}

// inScope is the honest coverage intersection: model-before ∧ authoritative ∧
// opted-in ∧ calibrated. Context-only and post-tool phases never enter payday.
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

// routeKey hashes the canonical state (cache.go) — normalized, byte-stable.
func routeKey(state string) string {
	sum := sha256.Sum256([]byte(canonicalState(state)))
	return hex.EncodeToString(sum[:])
}

// route runs Layer 0 → Layer 1 → Layer 2. Every failure is a transparent
// passthrough; no 503, no fail-closed, no raw conf/secret in the result.
func route(state string, p *pin, c *decisionCache) routeResult {
	var e stateEnvelope
	if json.Unmarshal([]byte(state), &e) != nil {
		return passResult("no-state", false)
	}

	// Layer 0: deterministic, µs — no SO call.
	if !inScope(e) {
		reason := "layer0-scope"
		if !e.OptIn {
			reason = "-32602" // capability missing: JSON-RPC invalid params code
		}
		return passResult(reason, false)
	}
	f := layer0{NeedLabel: e.NeedLabel, NoScratch: e.NoScratch, Safe: e.Safe, Reversible: e.Reversible}
	if !gate(f) {
		return passResult("layer0-gate", false)
	}
	if staticShortCircuit(e.State) {
		return passResult("layer0-static", false)
	}
	if !isLabelShaped(e.State) {
		return passResult("layer0-shape", false)
	}

	// Layer 1: cache hit or one SO shot.
	key := routeKey(state)
	if c != nil {
		if d, hit := c.lookup(key); hit {
			if d.Choice == abstainLabel {
				return passResult("cache-abstain", true)
			}
			return routeResult{Decision: decPin, Reason: "cache-hit", Covered: true, ConfBucket: d.ConfBucket}
		}
	}

	d := askSO(e.State) // only the cleaned user state crosses this boundary
	switch {
	case d.TimedOut:
		return passResult(classify(0, context.DeadlineExceeded), true)
	case d.Choice == "":
		return passResult("so-degraded", true) // HTTP≠200, parse error or conf<0.8
	case d.Choice != "yes":
		return passResult("so-"+d.Choice, true) // "no"/"other": abstain, never served
	case d.Conf < defaultThreshold():
		return passResult("so-low-conf", true)
	default:
		bucket := confBucket(d.Conf)
		if c != nil {
			c.admit(key, cachedDecision{Choice: d.Choice, ConfBucket: bucket, Reason: "margin=1"})
		}
		if p != nil {
			p.markSuccess(p.current().Name)
		}
		return routeResult{Decision: decPin, Reason: "so-admit", Covered: true, ConfBucket: bucket}
	}
}
