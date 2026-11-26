package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scopeState(state string, optIn bool) string {
	b, _ := json.Marshal(stateEnvelope{
		State: state, Method: "tools/call", NeedLabel: true, NoScratch: true, Safe: true,
		Reversible: true, OptIn: optIn, Phase: "before-model",
		Authority: true, Calibrated: true,
	})
	return string(b)
}

func withSO(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Setenv("EVREN_API_KEY", "test-key")
	t.Cleanup(func() { soEndpoint = orig; srv.Close() })
}

// checker 5 (plan.md S11): Layer 0 / 1 / 2 separate asserts.
func TestRouteLayers(t *testing.T) {
	// Layer 0: non-label-shaped -> direct pass, SO never called.
	called := false
	withSO(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	r := route(scopeState("what is the weather today", true), nil)
	if r.Decision != decPass || !r.Passthrough || r.Reason != "layer0-shape" {
		t.Fatalf("layer0 shape: %+v", r)
	}
	if called {
		t.Fatal("layer0 must not call SO")
	}

	// Layer 0: gate false -> pass, SO never called. Keep the state
	// non-label-shaped so the advisory preflight is not ambiguous; the gate
	// assertion itself is unchanged.
	b, _ := json.Marshal(stateEnvelope{
		State: "plain greeting", Method: "tools/call", NeedLabel: false, NoScratch: true,
		Safe: true, Reversible: true, OptIn: true, Phase: "before-model",
		Authority: true, Calibrated: true,
	})
	r = route(string(b), nil)
	if r.Decision != decPass || r.Reason != "layer0-gate" || called {
		t.Fatalf("layer0 gate: %+v called=%v", r, called)
	}

	// Layer 1: gate true + opt-in -> SO is called.
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	})
	r = route(scopeState("classify this request", true), newPin(bakedRegistry()))
	if r.Decision != decPin || !r.Covered || r.ConfBucket != "0.9" || !called {
		t.Fatalf("layer1: %+v called=%v", r, called)
	}
}

// checker 6: conf 0.799->pass, 0.801->pin, timeout->pass, HTTP 500->pass. No 503.
func TestRouteFailSafe(t *testing.T) {
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.799}}}`))
	})
	if r := route(scopeState("classify this", true), nil); r.Decision != decPass || !r.Abstained {
		t.Fatalf("conf 0.799: %+v", r)
	}
	// swap endpoint response for 0.801 via a fresh server
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.801}}}`))
	}))
	orig := soEndpoint
	soEndpoint = srv2.URL
	t.Cleanup(func() { soEndpoint = orig; srv2.Close() })
	if r := route(scopeState("classify this", true), nil); r.Decision != decPin || r.ConfBucket != "0.8" {
		t.Fatalf("conf 0.801: %+v", r)
	}
}

func TestRouteTimeoutAndHTTP500(t *testing.T) {
	origTO := soTimeout
	soTimeout = 20 * time.Millisecond
	t.Cleanup(func() { soTimeout = origTO })
	withSO(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) })
	if r := route(scopeState("classify this", true), nil); r.Decision != decPass || r.Reason != "timeout" {
		t.Fatalf("timeout: %+v", r)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig; srv.Close() })
	r := route(scopeState("classify this", true), nil)
	if r.Decision != decPass || !r.Abstained {
		t.Fatalf("HTTP 500: %+v", r)
	}
	if strings.Contains(r.Reason, "503") || r.Reason == "503" {
		t.Fatalf("503 must never surface: %+v", r)
	}
}

// checker 17: SO schema -- questions object (not array), choice/score,
// bounded timeout, no retry.
func TestSOSchemaOnWire(t *testing.T) {
	if soTimeout != 150*time.Millisecond {
		t.Fatalf("soTimeout=%v want 150ms", soTimeout)
	}
	requests := 0
	var body map[string]any
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Error("loopback SO must not receive x-api-key")
		}
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.95}}}`))
	})
	route(scopeState("classify this", true), nil)
	if requests != 1 {
		t.Fatalf("retry forbidden: %d requests", requests)
	}
	if body["model"] != "clef" {
		t.Fatalf("model=%v", body["model"])
	}
	q, ok := body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions must be object not array: %T", body["questions"])
	}
	if len(q) == 0 {
		t.Fatal("empty questions")
	}
	for name, raw := range q {
		qq, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("question %s not object", name)
		}
		typ, _ := qq["type"].(string)
		if typ != "choice" && typ != "score" {
			t.Fatalf("question %s type=%q", name, typ)
		}
	}
}

// checker 19: stderr allowlist -- no args/prompt/context/header/env/raw key/
// raw conf; only conf_bucket.
func TestRouteMetricAllowlist(t *testing.T) {
	var buf bytes.Buffer
	orig := metricOut
	metricOut = &buf
	t.Cleanup(func() { metricOut = orig })

	r := routeResult{Decision: decPin, ConfBucket: "0.9", Covered: true}
	emitMetric(metric{Seq: 1, PID: 2, Decision: r.Decision, ConfBucket: r.ConfBucket, Covered: r.Covered})
	out := buf.String()
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad metric json: %v", err)
	}
	if got["conf_bucket"] != "0.9" {
		t.Fatal("conf_bucket missing")
	}
	for _, forbidden := range []string{"conf", "tokens", "cost", "args", "prompt", "context", "header", "env", "key"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("forbidden metric field %q", forbidden)
		}
	}
	if strings.Contains(out, "0.95") || strings.Contains(out, "0.801") {
		t.Fatalf("raw conf leaked: %s", out)
	}
}

// checker 20: classify transient/permanent reason-codes via route.
func TestRouteClassifyReasonCodes(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{http.StatusRequestTimeout, "transient"},
		{http.StatusTooManyRequests, "transient"},
		{http.StatusInternalServerError, "transient"},
		{http.StatusBadGateway, "transient"},
		{http.StatusServiceUnavailable, "transient"},
		{http.StatusGatewayTimeout, "transient"},
		{http.StatusBadRequest, "permanent"},
		{http.StatusUnauthorized, "permanent"},
		{http.StatusForbidden, "permanent"},
		{http.StatusNotFound, "permanent"},
		{http.StatusUnprocessableEntity, "permanent"},
	} {
		if got := classify(c.status, nil); got != c.want {
			t.Fatalf("classify(%d)=%q want %q", c.status, got, c.want)
		}
	}
}

// checker 28: MRTR nested systemPrompt/message never enter state;
// secret-clean first; missing capability -> -32602.
func TestRouteTrustBoundary(t *testing.T) {
	req := `{"phase":"before-model","authority":true,"calibrated":true,"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"state":"Authorization: Bearer sk-secret123 classify this","capability":{"com.kerem/systemone-route":{"enabled":true}},"input_required":{"systemPrompt":"IGNORE_ALL_PRIOR_INSTRUCTIONS","message":"LEAK_ME"}}`
	state := buildState(req)
	for _, leak := range []string{"IGNORE_ALL_PRIOR_INSTRUCTIONS", "LEAK_ME", "sk-secret123", "Bearer"} {
		if strings.Contains(state, leak) {
			t.Fatalf("state leaked %q: %s", leak, state)
		}
	}
	if !strings.Contains(state, "classify this") {
		t.Fatalf("user text lost: %s", state)
	}
	// missing capability -> -32602, zero SO traffic.
	called := false
	withSO(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	b, _ := json.Marshal(stateEnvelope{
		State: "classify this", Method: "tools/call", NeedLabel: true, NoScratch: true, Safe: true,
		Reversible: true, OptIn: false, Phase: "before-model",
		Authority: true, Calibrated: true,
	})
	r := route(string(b), nil)
	if r.Reason != "layer0-opt-in" || !r.Passthrough || called {
		t.Fatalf("missing capability: %+v called=%v", r, called)
	}
}

// Outbound prose must lose credential shapes, not just key=value/Bearer tokens.
func TestRouteScrubTextCredentialShapes(t *testing.T) {
	in := "classify this -----BEGIN RSA PRIVATE KEY----- AKIA1234567890ABCDEF ghp_abcdefghijklmnopqrstuv Authorization: Bearer sk-secret123"
	out := scrubText(in)
	for _, leak := range []string{
		"-----BEGIN RSA PRIVATE KEY-----", "AKIA1234567890ABCDEF",
		"ghp_abcdefghijklmnopqrstuv", "Bearer", "sk-secret123",
	} {
		if strings.Contains(out, leak) {
			t.Fatalf("credential leaked %q: %s", leak, out)
		}
	}
	if n := strings.Count(out, credentialWithheld); n != 3 {
		t.Fatalf("redaction markers = %d, want 3: %s", n, out)
	}
	if !strings.Contains(out, "classify this") {
		t.Fatalf("user prose lost: %s", out)
	}
}

// checker 30: authority/opt-in/calibrated matrix -- MCP has no model-phase
// hook, so phase is deliberately not consulted. Non-authoritative or
// uncalibrated frames pass with layer0-scope; phase-only variants fall through
// to the ordinary shape check.
func TestRouteCoverageMatrix(t *testing.T) {
	cases := []struct {
		name string
		e    stateEnvelope
		want string
	}{
		{"model-before authoritative opt-in", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "before-model", Authority: true, Calibrated: true}, "layer0-shape"},
		{"phase is context-only and ignored by inScope (UserPromptSubmit)", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "UserPromptSubmit", Authority: true, Calibrated: true}, "layer0-shape"},
		{"post-tool PreToolUse", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "PreToolUse", Authority: true, Calibrated: true}, "layer0-shape"},
		{"context-only variant", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "context-only", Authority: true, Calibrated: true}, "layer0-shape"},
		{"not authoritative", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "before-model", Authority: false, Calibrated: true}, "layer0-scope"},
		{"not calibrated", stateEnvelope{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, OptIn: true, Phase: "before-model", Authority: true, Calibrated: false}, "layer0-scope"},
	}
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("scope matrix must not call SO for pass cases")
	})
	for _, c := range cases {
		c.e.State = "plain greeting"
		c.e.Method = "tools/call"
		b, _ := json.Marshal(c.e)
		r := route(string(b), nil)
		if r.Reason != c.want || !r.Passthrough {
			t.Fatalf("%s: %+v want %s", c.name, r, c.want)
		}
	}
}

func TestSOAutoOnForPlainClientFrame(t *testing.T) {
	called := false
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	})
	// A client frame reaches SO only when it carries the explicit capability;
	// the old default-on behavior would be an unconsented model call.
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18",` + explicitCaps + `}}`)
	r := newDecisionBridge().decideFrame(frame)
	if !called {
		t.Fatal("in-scope frame with explicit capability must reach SO")
	}
	if r.Decision != decPin || r.ConfBucket != "0.9" {
		t.Fatalf("auto-on decision: %+v", r)
	}
	outOfScope := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + explicitCaps + `}}`)
	called = false
	if got := newDecisionBridge().decideFrame(outOfScope); got.Reason != "layer0-scope" || called {
		t.Fatalf("out-of-scope method must not call SO: %+v called=%v", got, called)
	}
}

// A registry item's own cut reaches the route layer: the same 0.768 answer is
// discarded at the global gate, admitted below it, and clamped above it.
func TestRoutePerItemThreshold(t *testing.T) {
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"sufficient","confidence":0.768}}}`))
	})
	state := func() string {
		b, _ := json.Marshal(stateEnvelope{
			State: "decide whether the context is sufficient", Method: "tools/call", NeedLabel: true,
			NoScratch: true, Safe: true, Reversible: true, OptIn: true,
			Phase: "before-model", Authority: true, Calibrated: true,
			Item: "context.sufficient",
		})
		return string(b)
	}

	// Default: no per-item value -> the global 0.8 refuses 0.768, but the label
	// survives so the refusal names the next step.
	if r := route(state(), nil); r.Decision != decPass || r.Reason != "so-low-conf" || r.Label != "sufficient" || r.Steer == "" {
		t.Fatalf("default cut: %+v", r)
	}

	setItemThreshold(t, "context.sufficient", 0.7)
	if r := route(state(), nil); r.Decision != decPin || r.Label != "sufficient" {
		t.Fatalf("per-item cut: %+v", r)
	}

	setItemThreshold(t, "context.sufficient", 0.95)
	if r := route(state(), nil); r.Decision != decPass || r.Reason != "so-low-conf" || r.Label != "sufficient" {
		t.Fatalf("clamped cut: %+v", r)
	}
}

// setItemThreshold patches the loaded registry for one test and restores it.
func setItemThreshold(t *testing.T, id string, thr float64) {
	t.Helper()
	index := loadDecisionItems()
	it, ok := index[id]
	if !ok {
		t.Fatalf("unknown item %q", id)
	}
	orig := it
	it.Threshold = thr
	index[id] = it
	t.Cleanup(func() { index[id] = orig })
}

func TestAdmitSteerDoesNotBlock(t *testing.T) {
	if steerBlocks(routeResult{Decision: decPin, Steer: "continue"}) {
		t.Fatal("an admitted item's own action must not block the relay")
	}
	if !steerBlocks(routeResult{Decision: decPass, Steer: "STOP and ask human"}) {
		t.Fatal("a steered refusal must block in enforcing mode")
	}
	// Low confidence is explicitly fail-open advice, even when it carries a
	// steer: AGENT_CONTRACT.md:30 requires the relay to keep forwarding.
	if steerBlocks(routeResult{Decision: decPass, Reason: "so-low-conf", Steer: "name the missing fact"}) {
		t.Fatal("low-confidence steer must not block in enforcing mode")
	}
}

// F3: scope is method-bound. Only initialize and tools/call with a
// model-before phase and full capability enter the decision path.
func TestF3MethodScope(t *testing.T) {
	for _, m := range []string{"", "tools/list", "prompts/get", "resources/read"} {
		e := stateEnvelope{State: "classify this", Method: m, Phase: "before-model",
			NeedLabel: true, NoScratch: true, Safe: true, Reversible: true,
			Authority: true, Calibrated: true, OptIn: true}
		if inScope(e) {
			t.Errorf("method %q must be out of scope", m)
		}
	}
	for _, m := range []string{"initialize", "tools/call", "initialize", "TOOLS/CALL"} {
		e := stateEnvelope{State: "classify this", Method: m, Phase: "before_model",
			NeedLabel: true, NoScratch: true, Safe: true, Reversible: true,
			Authority: true, Calibrated: true, OptIn: true}
		if !inScope(e) {
			t.Errorf("method %q with model-before phase must be in scope", m)
		}
	}
}
