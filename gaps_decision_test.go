package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Gap tests added for the decision/gating hot paths. All helpers use the
// gapDecision prefix to stay isolated from other test files in package main.

func TestGapDecisionBucketConfBoundary(t *testing.T) {
	for _, tc := range []struct {
		bucket string
		want   float64
	}{
		{"1.0", 1},
		{"0.9", 0.9},
		{"0.8", 0.8},
		{"0.7", 0},
		{"", 0},
		{"raw-0.95", 0},
	} {
		if got := bucketConf(tc.bucket); got != tc.want {
			t.Errorf("bucketConf(%q) = %v, want %v", tc.bucket, got, tc.want)
		}
	}
}

func TestGapDecisionHoldActiveGenerationBoundary(t *testing.T) {
	d := &decisionBridge{}

	d.setHold(1, 0.8)
	if !d.holdActive() {
		t.Fatal("setHold must arm a live hold even at the 0.8 boundary")
	}
	if d.holdStrongFor(1) {
		t.Fatal("a 0.80 boundary admit must not arm the strong hysteresis hold")
	}
	if d.holdStrongFor(2) {
		t.Fatal("a hold must never leak into a different generation")
	}

	d.setHold(1, 0.81)
	if !d.holdStrongFor(1) {
		t.Fatal("confidence above 0.8 must arm the strong hold")
	}

	d.syncHold(2)
	if d.holdActive() {
		t.Fatal("syncHold on a generation change must clear the hold window")
	}
	if d.holdStrongFor(2) {
		t.Fatal("syncHold on a generation change must clear the strong hold")
	}
}

func TestGapDecisionWithSteerEmpty(t *testing.T) {
	if got := withSteer("blocked", ""); got != "blocked" {
		t.Fatalf("withSteer with empty steer = %q, want %q", got, "blocked")
	}
	if got := withSteer("blocked", "ask human"); got != "blocked: ask human" {
		t.Fatalf("withSteer with steer = %q, want %q", got, "blocked: ask human")
	}
}

func TestGapDecisionRPCIDNullFallback(t *testing.T) {
	for _, id := range [][]byte{nil, {}} {
		out := rpcError(id, -32001, "blocked")
		if len(out) == 0 || out[len(out)-1] != '\n' {
			t.Fatalf("rpcError must emit one newline-terminated frame, got %q", out)
		}
		var p rpcErrorPayload
		if err := json.Unmarshal(out, &p); err != nil {
			t.Fatalf("rpcError emitted invalid JSON %q: %v", out, err)
		}
		if p.JSONRPC != "2.0" || string(p.ID) != "null" || p.Error.Code != -32001 || p.Error.Message != "blocked" {
			t.Fatalf("rpcError payload = %+v (id=%s), want 2.0/null/-32001/blocked", p, p.ID)
		}
	}
}

func TestGapDecisionUnwrapParamsNonObjectFallback(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1,2]}`)
	f, err := decodeFrame(raw)
	if err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	got := unwrapParams(raw, f)
	if string(got) != `[1,2]` {
		t.Fatalf("unwrapParams with non-object params = %q, want %q", got, `[1,2]`)
	}
}

func TestGapDecisionEnforcingGateMalformedFrame(t *testing.T) {
	d := &decisionBridge{}
	out, blocked := d.enforcingGate([]byte("not json"))
	if out != nil || blocked {
		t.Fatalf("malformed frame must fail open before any gate: out=%q blocked=%v", out, blocked)
	}
}

func TestGapDecisionSameSOEndpointIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"case-insensitive scheme and host", "https://EXAMPLE.com/v1", "HTTPS://example.COM/v1", true},
		{"explicit port is identity", "https://example.com/v1", "https://example.com:443/v1", false},
		{"path is identity", "https://example.com/v1", "https://example.com/v2", false},
		{"unparseable equal", "%zz", "%zz", true},
		{"unparseable different", "%zz", "%yy", false},
	} {
		if got := sameSOEndpoint(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameSOEndpoint(%q,%q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestGapDecisionStringListFieldShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  map[string]any
		keys []string
		want []string
	}{
		{
			name: "drops numbers and blanks",
			obj:  map[string]any{"criteria": []any{"yes", 7, "  ", "no"}, "evidence": []any{"e"}},
			keys: []string{"criteria", "evidence"},
			want: []string{"yes", "no"},
		},
		{
			name: "empty array falls through to next key",
			obj:  map[string]any{"criteria": []any{7, " "}, "evidence": []any{"e1", nil}},
			keys: []string{"criteria", "evidence"},
			want: []string{"e1"},
		},
		{
			name: "non-array skipped",
			obj:  map[string]any{"criteria": "yes", "evidence": []any{"e"}},
			keys: []string{"criteria", "evidence"},
			want: []string{"e"},
		},
		{
			name: "all empty returns nil",
			obj:  map[string]any{"criteria": []any{}, "evidence": []any{}},
			keys: []string{"criteria", "evidence"},
			want: nil,
		},
	} {
		got := stringListField(tc.obj, tc.keys...)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: stringListField = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestGapDecisionInvalidSOProbabilitiesShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    map[string]float64
		want bool
	}{
		{"absent map uses legacy path", nil, false},
		{"single label", map[string]float64{"yes": 1}, true},
		{"NaN", map[string]float64{"yes": math.NaN(), "no": 1}, true},
		{"positive infinity", map[string]float64{"yes": math.Inf(1), "no": 0}, true},
		{"negative value", map[string]float64{"yes": -0.1, "no": 1.1}, true},
		{"above one", map[string]float64{"yes": 1.1, "no": -0.1}, true},
		{"sum too high", map[string]float64{"yes": 0.6, "no": 0.42}, true},
		{"sum within tolerance", map[string]float64{"yes": 0.6, "no": 0.395}, false},
	} {
		if got := invalidSOProbabilities(tc.p); got != tc.want {
			t.Errorf("%s: invalidSOProbabilities(%v) = %v, want %v", tc.name, tc.p, got, tc.want)
		}
	}
}

func TestGapDecisionQuestionApplicableRequires(t *testing.T) {
	q := soQuestion{Type: "choice", Requires: []string{"untrusted_text"}}
	for _, tc := range []struct {
		name  string
		q     soQuestion
		state string
		want  bool
	}{
		{"requirement present", q, `{"untrusted_text":"x"}`, true},
		{"requirement absent", q, `{"state":"x"}`, false},
		{"non-JSON state", q, "not json", false},
		{"no requirements", soQuestion{Type: "choice"}, "not json", true},
	} {
		if got := questionApplicable(tc.q, tc.state); got != tc.want {
			t.Errorf("%s: questionApplicable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGapDecisionValidSOAnswerShapeEdges(t *testing.T) {
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}
	a, reason, ok := validSOAnswer(soAnswer{Type: "choice", Choice: "  ", Confidence: 0.9}, q)
	if ok || reason != "invalid-response" {
		t.Fatalf("blank choice: ok=%v reason=%q, want false/invalid-response", ok, reason)
	}
	if a.Choice != "" {
		t.Fatalf("blank choice was rewritten to %q", a.Choice)
	}

	variantQ := soQuestion{Type: "choice", Criteria: map[string]string{"YES": "admit", "NO": "pass"}}
	a, reason, ok = validSOAnswer(soAnswer{Type: "choice", Choice: "yes", Confidence: 0.9}, variantQ)
	if !ok || reason != "" || a.Choice != "yes" {
		t.Fatalf("case-variant label: choice=%q reason=%q ok=%v, want yes/''/true", a.Choice, reason, ok)
	}

	a, reason, ok = validSOAnswer(soAnswer{Type: "choice", Choice: "yes", Confidence: 0.9,
		Probabilities: map[string]float64{"": 0.5, "yes": 0.5}}, q)
	if ok || reason != "invalid_probabilities" {
		t.Fatalf("blank probability label: ok=%v reason=%q, want false/invalid_probabilities", ok, reason)
	}
}

func TestGapDecisionKnownRegistryLabelEmpty(t *testing.T) {
	if knownRegistryLabel("") {
		t.Fatal("empty label must never be a registry label")
	}
	if knownRegistryLabel("definitely-not-a-registry-label-zz") {
		t.Fatal("unknown label must not be a registry label")
	}
	var known []string
	for _, it := range loadDecisionItems() {
		for label := range it.Q.Labels {
			known = append(known, label)
		}
	}
	sort.Strings(known)
	if len(known) == 0 {
		t.Fatal("embedded registry has no labels")
	}
	if !knownRegistryLabel(known[0]) {
		t.Fatalf("registry label %q was not recognized", known[0])
	}
}

func TestGapDecisionAnswerMarginShort(t *testing.T) {
	for _, p := range []map[string]float64{nil, {"yes": 0.9}} {
		m, ok := answerMargin(p)
		if ok || m != 0 {
			t.Fatalf("answerMargin(%v) = (%v,%v), want (0,false)", p, m, ok)
		}
	}
	m, ok := answerMargin(map[string]float64{"yes": 0.6, "other": 0.3, "no": 0.1})
	if !ok || math.Abs(m-0.3) > 1e-9 {
		t.Fatalf("answerMargin with distribution = (%v,%v), want (0.3,true)", m, ok)
	}
}

func TestGapDecisionStaleRenameCallerScan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  bool
	}{
		{"deleted declaration", "-func gone() {}\n", true},
		{"deleted declaration with caller scan", "-func gone() {}\nCALLER SCAN: gone still used\n", false},
		{"no declaration change", "unchanged text", false},
	} {
		if got := staleRename(tc.state); got != tc.want {
			t.Errorf("%s: staleRename = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGapDecisionLedgerMetadataMissingShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		want  []string
	}{
		{"malformed state", "not json", []string{"who", "when"}},
		{"absent owner and round", `{"done":["x"]}`, []string{"who", "when"}},
		{"null and empty object", `{"who":null,"when":{}}`, []string{"who", "when"}},
		{"both present", `{"who":"alice","when":"round 1"}`, nil},
	} {
		got := ledgerMetadataMissing(tc.state)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ledgerMetadataMissing = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

func TestGapDecisionScrubStringsNestedAndInvalidMirror(t *testing.T) {
	in := map[string]any{"nested": []any{
		map[string]any{"note": "api_key=supersecret"}, "hello", 7,
	}}
	out, ok := scrubStrings(in).(map[string]any)
	if !ok {
		t.Fatalf("scrubStrings returned %T, want map[string]any", scrubStrings(in))
	}
	arr, ok := out["nested"].([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("nested slice = %#v, want 3 elements", out["nested"])
	}
	m, ok := arr[0].(map[string]any)
	if !ok || m["note"] != "" {
		t.Fatalf("keyed secret in nested map = %#v, want empty", arr[0])
	}
	if arr[1] != "hello" || arr[2] != 7 {
		t.Fatalf("non-secret leaves changed: %#v", arr[1:])
	}
	if got := string(scrubbedMirror([]byte("not json"))); got != `{}` {
		t.Fatalf("scrubbedMirror invalid JSON = %q, want {}", got)
	}
}

func TestGapDecisionMirrorCanaryLoopback(t *testing.T) {
	bodyCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mirrorCanary(srv.URL, []byte(`{"state":"api_key=supersecret and hello"}`))
	select {
	case body := <-bodyCh:
		if !strings.Contains(body, `"and hello"`) {
			t.Fatalf("mirror body lost surviving text: %q", body)
		}
		if strings.Contains(body, "supersecret") {
			t.Fatalf("mirror body leaked secret: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mirror canary did not POST to loopback server")
	}
}

func TestGapDecisionCanonicalStateRawFallback(t *testing.T) {
	raw := canonicalState("not json")
	if !strings.HasPrefix(raw, "raw:") {
		t.Fatalf("canonicalState of prose = %q, want raw: prefix", raw)
	}
	if raw != canonicalState("not json") {
		t.Fatal("canonicalState of identical prose must be deterministic")
	}
	if raw == canonicalState("other prose") || raw == canonicalState(`{"state":"not json"}`) {
		t.Fatal("raw canonical form must not collide with other text or JSON")
	}
}

func TestGapDecisionNeedsPrewarmEviction(t *testing.T) {
	c := newShardedCache()
	for i := 0; i < 4*cacheCapacity+1; i++ {
		var k [32]byte
		binary.BigEndian.PutUint32(k[:], uint32(i))
		c.warmHits[k] = 1
		c.warmAt[k] = time.Now()
		c.warmDec[k] = cachedDecision{Choice: "yes"}
		c.warmExpires[k] = time.Now().Add(time.Hour)
		c.refreshing[k] = true
	}

	var k [32]byte
	k[0] = 0xAA
	if !c.needsPrewarm(k, 1, time.Minute) {
		t.Fatal("first hot lookup must request prewarm")
	}
	if len(c.warmHits) != 1 || len(c.warmAt) != 1 || len(c.warmDec) != 0 || len(c.warmExpires) != 0 {
		t.Fatalf("oversized warm maps not reset: hits=%d at=%d dec=%d expires=%d",
			len(c.warmHits), len(c.warmAt), len(c.warmDec), len(c.warmExpires))
	}
	if !c.refreshing[k] || len(c.refreshing) != 1 {
		t.Fatalf("refresh map after reset = %#v, want only the new key", c.refreshing)
	}
}
