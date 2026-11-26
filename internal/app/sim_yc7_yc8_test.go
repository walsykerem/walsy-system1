package app

// Genuine simulation for MANIFEST YC7 and YC8 (batch B34). No production file
// is edited. Every case drives the real SO POST path, the real strict decoder
// and the real decision bridge against an httptest oracle stub. Assertions
// encode the REQUIRED behavior, so a missing or weakened feature fails here.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	simYCReq  = `{"state":"decide: classify this","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`
	simYCCaps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`
)

type simYCOracle struct {
	mu     sync.Mutex
	posts  int
	bodies []string
	reply  func(int) string
}

func (s *simYCOracle) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.posts++
	s.bodies = append(s.bodies, string(body))
	n, reply := s.posts, s.reply
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, reply(n))
}

func (s *simYCOracle) setReply(f func(int) string) {
	s.mu.Lock()
	s.reply = f
	s.mu.Unlock()
}

func (s *simYCOracle) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func simYCAnswer(choice string, conf float64) string {
	b, _ := json.Marshal(map[string]any{
		"model": "clef",
		"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}},
	})
	return string(b)
}

// simYCBase makes HOME hermetic, clears config mode, never reads a real key,
// and attaches a stub to the production oracle transport.
func simYCBase(t *testing.T) *simYCOracle {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "sim-yc7-yc8-fake-key")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	routerReset()
	t.Cleanup(routerReset)
	resetLogs()
	t.Cleanup(resetLogs)

	st := &simYCOracle{reply: func(int) string { return simYCAnswer("yes", 0.95) }}
	srv := httptest.NewServer(http.HandlerFunc(st.serve))
	oldEP, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEP, oldClient
		srv.Close()
	})
	return st
}

func simYCStored(t *testing.T, d *decisionBridge) bool {
	t.Helper()
	k := d.cacheKey([]byte(simYCReq), soCapability(), d.pin.generation())
	s := d.cache.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.idx[hex.EncodeToString(k[:])]
	return ok
}

// YC7: a question carrying requires=[tool_result] is inapplicable when the
// state has no tool_result. It must be dropped before the POST, so no request
// reaches SO and no decision is produced.
func TestSimYC7RequiresSuppressesPost(t *testing.T) {
	st := simYCBase(t)
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"},
		Requires: []string{"tool_result"}}
	state := `{"state":"decide: indirect injection check","method":"tools/call",` + simYCCaps + `}`
	d := askSOQOnce(state, q)
	if st.count() != 0 || d.Choice != "" {
		t.Fatalf("unmet requires must skip the POST: posts=%d choice=%q decision=%+v", st.count(), d.Choice, d)
	}
}

// YC7 control: the same question with the required key present must be asked.
func TestSimYC7RequiresSatisfiedPosts(t *testing.T) {
	st := simYCBase(t)
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"},
		Requires: []string{"tool_result"}}
	state := `{"state":"decide: indirect injection check","tool_result":"untrusted","method":"tools/call",` + simYCCaps + `}`
	d := askSOQOnce(state, q)
	if st.count() != 1 || d.Choice != "yes" {
		t.Fatalf("satisfied requires must post once and admit: posts=%d choice=%q decision=%+v", st.count(), d.Choice, d)
	}
}

// YC7 via real registry data: intake.injection declares
// state_fields=[untrusted_text,source]. With neither field in state the item's
// question is inapplicable and the single POST must be skipped.
func TestSimYC7ItemStateFieldsGate(t *testing.T) {
	st := simYCBase(t)
	st.setReply(func(int) string { return simYCAnswer("clean", 0.95) })
	item, ok := decisionItemFor("intake.injection")
	if !ok {
		t.Fatal("intake.injection missing from embedded registry")
	}
	if len(item.StateFields) == 0 {
		t.Fatal("intake.injection has no state_fields to gate on")
	}
	p := newPin(bakedRegistry())
	missing := `{"state":"decide: untrusted input","item":"intake.injection","method":"tools/call",` + simYCCaps + `}`
	r := route(missing, p)
	if st.count() != 0 {
		t.Fatalf("state_fields absent but SO was called %d times: %+v", st.count(), r)
	}
	// Control: present required fields must reach SO through the same route.
	present := `{"state":"decide: untrusted input","item":"intake.injection","untrusted_text":"x","source":"tool","method":"tools/call",` + simYCCaps + `}`
	r2 := route(present, p)
	if st.count() != 1 {
		t.Fatalf("state_fields present but SO posts=%d: %+v", st.count(), r2)
	}
	if r2.Decision != decPin {
		t.Fatalf("satisfied item decision = %+v, want admit", r2)
	}
}

// YC8: a duplicate top-level key in the SO answer must be refused before a
// decision is returned or a cache cell is written. The next identical request
// therefore has to ask SO again.
func TestSimYC8DuplicateRejectedBeforeCache(t *testing.T) {
	st := simYCBase(t)
	st.setReply(func(int) string {
		return `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}},"usage":{},"usage":{}}`
	})
	d := newDecisionBridge()
	r := d.decide([]byte(simYCReq))
	if r.Decision == decPin || r.Reason != "so-degraded" {
		t.Fatalf("duplicate-key answer accepted: %+v", r)
	}
	if simYCStored(t, d) {
		t.Fatal("duplicate-key answer was written into the decision cache")
	}
	st.setReply(func(int) string { return simYCAnswer("yes", 0.95) })
	r2 := d.decide([]byte(simYCReq))
	if st.count() != 2 || r2.Reason != "so-admit" {
		t.Fatalf("rejected decision leaked into next call: posts=%d result=%+v", st.count(), r2)
	}
}

// YC8: strict SO decode rejects duplicate, case-variant and unknown top-level
// keys, duplicates inside the answer object, two documents and a 1 MiB body.
func TestSimYC8StrictResponseDecode(t *testing.T) {
	cases := map[string]string{
		"duplicate-top":    `{"answers":{},"answers":{}}`,
		"case-variant-top": `{"answers":{},"ANSWERS":{}}`,
		"unknown-top":      `{"answers":{},"routed_model":"other"}`,
		"duplicate-nested": `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95,"CONFIDENCE":0.1}}}`,
		"two-documents":    `{"answers":{}} {"answers":{}}`,
		"over-1mib":        string(bytes.Repeat([]byte("x"), 1<<20+16)),
	}
	for name, raw := range cases {
		st := simYCBase(t)
		body := raw
		st.setReply(func(int) string { return body })
		got := askSOQOnce("decide: classify this", soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}})
		if st.count() != 1 {
			t.Fatalf("%s: SO was not reached through the real transport", name)
		}
		if got.Choice != "" {
			t.Errorf("%s: malformed SO answer produced a decision: %+v", name, got)
		}
	}
	st := simYCBase(t)
	st.setReply(func(int) string { return simYCAnswer("yes", 0.95) })
	if got := askSOQOnce("decide: classify this", soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}); got.Choice != "yes" {
		t.Fatalf("valid control answer rejected: %+v", got)
	}
}

// YC8: the inbound JSON-RPC trust boundary rejects a case-variant duplicate
// inside params.criteria (orderedObject equivalent) before any SO POST.
func TestSimYC8InboundDuplicateCriteria(t *testing.T) {
	st := simYCBase(t)
	d := newDecisionBridge()
	bad := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"decide: classify this","criteria":{"yes":"a","YES":"b"},` + simYCCaps + `}}`
	r := d.decideFrame([]byte(bad))
	if r.Reason != "no-state" || st.count() != 0 {
		t.Fatalf("duplicate criteria reached the gate: reason=%s posts=%d", r.Reason, st.count())
	}
	good := strings.Replace(bad, `"criteria":{"yes":"a","YES":"b"},`, "", 1)
	r2 := d.decideFrame([]byte(good))
	if st.count() != 1 || r2.Reason != "so-admit" {
		t.Fatalf("control frame did not pass the gate: reason=%s posts=%d", r2.Reason, st.count())
	}
}

// YC8: a frame above the 8 MiB transport budget is refused with no SO POST.
func TestSimYC8OversizedFrameNoPost(t *testing.T) {
	st := simYCBase(t)
	d := newDecisionBridge()
	r := d.decideFrame(bytes.Repeat([]byte("x"), maxHTTPFrame+16))
	if r.Reason != "frame-too-large" || !r.Passthrough || st.count() != 0 {
		t.Fatalf("oversized frame not rejected at boundary: reason=%s passthrough=%v posts=%d", r.Reason, r.Passthrough, st.count())
	}
}

// YC8: inbound frames use the 8 MiB transport budget, not the 1 MiB SO reply cap.
func TestSimYC8FrameBudgetMatchesTransport(t *testing.T) {
	good := []byte(`{"method":"x","params":{"pad":"` + strings.Repeat("a", 2<<20) + `"}}`)
	f, err := decodeFrame(good)
	if err != nil {
		t.Fatalf("valid 2 MiB frame rejected: %v", err)
	}
	if f.Method != "x" || len(f.Params) < 2<<20 {
		t.Fatalf("2 MiB frame decoded wrong: method=%q params=%d", f.Method, len(f.Params))
	}
	if _, err := decodeFrame(bytes.Repeat([]byte("x"), maxHTTPFrame+16)); err == nil {
		t.Fatal("decodeFrame accepted a frame above the 8 MiB transport budget")
	}
}

// YC8: the decision boundary uses the 8 MiB transport budget, not the 1 MiB SO
// reply cap. A valid 2 MiB frame must reach SO; a frame above 8 MiB must not.
func TestSimYC8DecisionBoundaryIs8MiB(t *testing.T) {
	st := simYCBase(t)
	d := newDecisionBridge()
	pad := strings.Repeat("a", 2<<20)
	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"decide: classify this","pad":"` + pad + `",` + simYCCaps + `}}`)
	r, dec := d.decideFrameDetailed(raw)
	if r.Reason == "frame-too-large" {
		t.Fatalf("valid %d-byte frame refused as frame-too-large", len(raw))
	}
	if st.count() != 1 || r.Reason != "so-admit" || dec.Choice != "yes" {
		t.Fatalf("2 MiB frame did not reach SO: posts=%d reason=%s decision=%+v", st.count(), r.Reason, dec)
	}

	over, _ := d.decideFrameDetailed(bytes.Repeat([]byte("x"), maxHTTPFrame+16))
	if over.Reason != "frame-too-large" || !over.Passthrough || st.count() != 1 {
		t.Fatalf("oversized frame passed decision boundary: reason=%s passthrough=%v posts=%d", over.Reason, over.Passthrough, st.count())
	}
}
