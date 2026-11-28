package app

// fix4-12: hermetic, deterministic fallback/combo simulation. All package-level
// names use the fix4_12_ prefix. No production file is touched; defects found
// here are reported to the owning slices (05 combo.go, 03 gate.go).
// Every SO endpoint is loopback; the remote backstop URL is rewritten inside
// the injected HTTP client, so no test ever reaches the network.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fix4_12_primary   = "Fix4-Primary"
	fix4_12_secondary = "Fix4-Secondary"
	fix4_12_frame     = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"classify fallback frame","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}` + "\n"
)

type fix4_12_stub struct {
	mu     sync.Mutex
	posts  int
	bodies []string
	reply  func(int) (int, string)
}

func fix4_12_server(t *testing.T, reply func(int) (int, string)) (*httptest.Server, *fix4_12_stub) {
	t.Helper()
	st := &fix4_12_stub{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		st.mu.Lock()
		st.posts++
		st.bodies = append(st.bodies, string(b))
		n := st.posts
		st.mu.Unlock()
		code, body := st.reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func (s *fix4_12_stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func (s *fix4_12_stub) wire() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// fix4_12_rewrite maps the default (remote) System One base onto a loopback
// stub. Keeping soEndpoint at its default is required: an explicit override
// collapses soPool() to a single candidate and would hide failover entirely.
type fix4_12_rewrite struct {
	to    string
	inner http.RoundTripper
}

func (rt fix4_12_rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" {
		return rt.inner.RoundTrip(req)
	}
	u, err := url.Parse(rt.to)
	if err != nil {
		return nil, err
	}
	cp := req.Clone(req.Context())
	cp.URL.Scheme, cp.URL.Host, cp.URL.Path, cp.URL.RawQuery = u.Scheme, u.Host, u.Path, ""
	cp.Host = u.Host
	return rt.inner.RoundTrip(cp)
}

func fix4_12_base(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_APPROVER", "")
	t.Setenv("EVREN_API_KEY", "fix4-12-key")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	routerReset()
}

func fix4_12_attach(t *testing.T, backstop string) {
	t.Helper()
	ep, cl := soEndpoint, soClient
	soEndpoint = soEndpointDefaultBase
	soClient = &http.Client{Transport: fix4_12_rewrite{to: backstop, inner: http.DefaultTransport}}
	t.Cleanup(func() { soEndpoint, soClient = ep, cl })
}

func fix4_12_setup(t *testing.T, primary, secondary, backstop *httptest.Server, primaryName, secondaryName string) {
	t.Helper()
	for _, p := range []struct{ name, base string }{
		{primaryName, primary.URL + "/v1/systemone"},
		{secondaryName, secondary.URL + "/v1/systemone"},
	} {
		if err := saveCustomProvider(p.name, p.base, "FIX4_12_UNSET_KEY"); err != nil {
			t.Fatalf("saveCustomProvider(%q): %v", p.name, err)
		}
	}
	if err := saveCombo([]stateStep{
		{Provider: primaryName, Model: "fix4-primary", Weight: 2},
		{Provider: secondaryName, Model: "fix4-secondary", Weight: 1},
	}); err != nil {
		t.Fatalf("saveCombo: %v", err)
	}
	fix4_12_attach(t, backstop.URL+"/v1/systemone")
}

func fix4_12_attempts() int {
	soAttemptMu.Lock()
	defer soAttemptMu.Unlock()
	return soAttemptN
}

// fix4_12_wire returns the candidate's semantic state and SO question set.
// The so_cache_contract is model-bound by design (soStableIDs hashes the model),
// so failover may change its hashes; the state and intent must not change.
func fix4_12_wire(t *testing.T, body string, questions map[string]json.RawMessage) string {
	t.Helper()
	var post struct {
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal([]byte(body), &post); err != nil {
		t.Fatalf("SO POST is not JSON: %v (%s)", err, body)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal([]byte(post.State), &state); err != nil {
		t.Fatalf("wire state is not JSON: %v (%s)", err, post.State)
	}
	contract, ok := state["so_cache_contract"]
	if !ok || !strings.Contains(string(contract), "so_session_id=") || !strings.Contains(string(contract), "so_prompt_cache_key=") {
		t.Fatalf("wire state lost the cache contract: %s", post.State)
	}
	delete(state, "so_cache_contract")
	semantic, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(post.State, "classify this request") ||
		!strings.Contains(post.State, `"need_label":true`) ||
		!strings.Contains(post.State, `"opt_in":true`) ||
		!strings.Contains(post.State, `"phase":"before-model"`) {
		t.Fatalf("wire state lost intent/capabilities: %s", post.State)
	}
	for k, v := range post.Questions {
		questions[k] = v
	}
	return string(semantic)
}

func fix4_12_readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// GREEN: each primary failure mode hands the decision to the secondary with
// one POST per candidate, no backstop POST, one SO attempt, and no state loss.
func TestFix4_12FallbackGreenSecondaryTakesOver(t *testing.T) {
	cases := []struct {
		name string
		fail func(int) (int, string)
	}{
		{"http_500", func(int) (int, string) { return 500, `{"error":"primary down"}` }},
		{"timeout", func(int) (int, string) {
			time.Sleep(300 * time.Millisecond)
			return 200, w11_reply("yes", 0.95)(1)
		}},
		{"malformed_json", func(int) (int, string) { return 200, `{"answers":` }},
		{"out_of_set", func(int) (int, string) {
			return 200, `{"answers":{"gate":{"type":"choice","choice":"out-of-set","confidence":0.9}}}`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix4_12_base(t)
			primary, ps := fix4_12_server(t, tc.fail)
			secondary, ss := fix4_12_server(t, func(int) (int, string) { return 200, w11_reply("yes", 0.95)(1) })
			backstop, bs := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
			fix4_12_setup(t, primary, secondary, backstop, fix4_12_primary, fix4_12_secondary)

			before := fix4_12_attempts()
			d := newDecisionBridge()
			w11_primePin(d)
			r := d.decide([]byte(w11_req))
			if r.Decision != decPin || r.Reason != "so-admit" || r.LastDecision.Choice != "yes" {
				t.Fatalf("fallback decision = %+v, want pin/so-admit from secondary", r)
			}
			if ps.count() != 1 || ss.count() != 1 || bs.count() != 0 {
				t.Fatalf("posts primary=%d secondary=%d backstop=%d, want 1/1/0", ps.count(), ss.count(), bs.count())
			}
			if got := fix4_12_attempts() - before; got != 1 {
				t.Fatalf("SO attempts = %d, want 1 (no restart of the chain)", got)
			}

			pw, sw := ps.wire(), ss.wire()
			pq, sq := map[string]json.RawMessage{}, map[string]json.RawMessage{}
			pState := fix4_12_wire(t, pw[0], pq)
			sState := fix4_12_wire(t, sw[0], sq)
			if pState != sState {
				t.Fatalf("state/intent changed across failover:\nprimary  %s\nsecondary %s", pState, sState)
			}
			pqj, _ := json.Marshal(pq)
			sqj, _ := json.Marshal(sq)
			if string(pqj) != string(sqj) || len(pq) == 0 {
				t.Fatalf("question set changed across failover: %s vs %s", pqj, sqj)
			}
		})
	}
}

// RED: with every candidate unusable, advisory mode abstains explicitly and
// the chain stops after exactly one POST per candidate; nothing is cached.
func TestFix4_12FallbackRedAllFailExplicitAbstain(t *testing.T) {
	fix4_12_base(t)
	primary, ps := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	secondary, ss := fix4_12_server(t, func(int) (int, string) { return 200, `{"answers":` })
	backstop, bs := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	fix4_12_setup(t, primary, secondary, backstop, fix4_12_primary, fix4_12_secondary)

	before := fix4_12_attempts()
	d := newDecisionBridge()
	r := d.decide([]byte(w11_req))
	if r.Decision != decPass || !r.Abstained || r.Reason != "so-degraded" {
		t.Fatalf("all-fail decision = %+v, want explicit pass/so-degraded abstain", r)
	}
	if r.Steer == "" {
		t.Fatal("abstain has no steer: ambiguous success")
	}
	if r.LastDecision.Choice != "" || r.LastDecision.Conf != 0 {
		t.Fatalf("degraded answer carries a choice: %+v", r.LastDecision)
	}
	if ps.count() != 1 || ss.count() != 1 || bs.count() != 1 {
		t.Fatalf("posts primary=%d secondary=%d backstop=%d, want 1/1/1 (chain terminates)", ps.count(), ss.count(), bs.count())
	}
	if got := fix4_12_attempts() - before; got != 1 {
		t.Fatalf("SO attempts = %d, want 1", got)
	}
	key := d.cacheKey([]byte(w11_req), soCapability(), d.pin.generation())
	s := d.cache.shard(key)
	s.mu.Lock()
	_, cached := s.idx[hex.EncodeToString(key[:])]
	s.mu.Unlock()
	if cached {
		t.Fatal("degraded all-fail outcome was cached")
	}
}

// RED in enforcing mode: no candidate answers -> explicit deny, never an admit.
func TestFix4_12FallbackRedEnforcingDenies(t *testing.T) {
	fix4_12_base(t)
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("WALSY_SO_APPROVER", "fix4-12")
	primary, ps := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	secondary, ss := fix4_12_server(t, func(int) (int, string) { return 200, `{"answers":` })
	backstop, bs := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	// Enforcing pins the oracle: "Flide Local" may move its port (the only
	// baked-equivalent override), a renamed slot is skipped, then the backstop.
	fix4_12_setup(t, primary, secondary, backstop, "Flide Local", fix4_12_secondary)

	d := newDecisionBridge()
	r := d.decide([]byte(w11_req))
	if r.Decision != decDeny || !r.Abstained || r.Reason != "so-degraded" || r.Steer == "" {
		t.Fatalf("enforcing all-fail = %+v, want explicit deny/so-degraded", r)
	}
	if ps.count() != 1 || ss.count() != 0 || bs.count() != 1 {
		t.Fatalf("posts primary=%d secondary=%d backstop=%d, want 1/0/1 (renamed slot skipped)", ps.count(), ss.count(), bs.count())
	}
}

// The applied decision is attributable: audit names the secondary provider,
// and exactly one chained receipt exists for the frame (no duplicate side effect).
func TestFix4_12FallbackAuditNamesWinnerAndChainsOnce(t *testing.T) {
	fix4_12_base(t)
	primary, _ := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	secondary, ss := fix4_12_server(t, func(int) (int, string) { return 200, w11_reply("yes", 0.95)(1) })
	backstop, _ := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	fix4_12_setup(t, primary, secondary, backstop, fix4_12_primary, fix4_12_secondary)

	d := newDecisionBridge()
	w11_primePin(d)
	r, _ := d.decideFrameDetailed([]byte(fix4_12_frame))
	if r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("frame decision = %+v, want pin/so-admit", r)
	}
	if ss.count() != 1 {
		t.Fatalf("secondary posts = %d, want 1", ss.count())
	}

	home, _ := os.UserHomeDir()
	audit := fix4_12_readLines(t, filepath.Join(home, routerDir, routerAuditName))
	found, leaked := false, false
	for _, line := range audit {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Fatalf("bad audit line: %s", line)
		}
		if rec["event"] != "gate" {
			continue
		}
		if rec["provider"] == fix4_12_secondary {
			found = true
		}
		if rec["provider"] == fix4_12_primary {
			leaked = true
		}
	}
	if !found || leaked {
		t.Fatalf("audit winner = secondary:%v primary-leak:%v; records: %v", found, leaked, audit)
	}

	chain := fix4_12_readLines(t, filepath.Join(home, routerDir, "receipt.chain"))
	decisions := 0
	for _, line := range chain {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Fatalf("bad receipt line: %s", line)
		}
		if rec["event"] == "decision" {
			decisions++
			if rec["decision"] != "pin" || rec["admit_deny"] != "admit" || rec["outcome"] != "so-admit" {
				t.Fatalf("receipt does not reflect the applied fallback: %s", line)
			}
		}
	}
	if decisions != 1 {
		t.Fatalf("decision receipts = %d, want 1 (no duplicated side effect)", decisions)
	}
}

// Combo-level fallback: the secondary step is the winner when the primary
// fails, and the loop stops in round 1. A 2xx with a malformed SO body no
// longer wins: SO-shaped endpoints must return at least one answers object.
func TestFix4_12ComboFallbackStepAndInvalidBodyDefect(t *testing.T) {
	fix4_12_base(t)
	good := func(int) (int, string) { return 200, w11_reply("yes", 0.95)(1) }
	primary, ps := fix4_12_server(t, func(int) (int, string) { return 500, `{}` })
	secondary, ss := fix4_12_server(t, good)
	steps := []ComboStep{
		{Name: fix4_12_primary, Base: primary.URL, Key: "p", Model: "fix4-primary", Weight: 2},
		{Name: fix4_12_secondary, Base: secondary.URL, Key: "s", Model: "fix4-secondary", Weight: 1},
	}
	out := RunCombo(steps, 300*time.Millisecond)
	if out.Winner != fix4_12_secondary || out.Rounds != 1 || len(out.Steps) != 2 || out.Steps[0].OK || !out.Steps[1].OK {
		t.Fatalf("combo fallback = %+v, want secondary winner in round 1", out)
	}
	if ps.count() != 1 || ss.count() != 1 {
		t.Fatalf("combo posts primary=%d secondary=%d, want 1/1", ps.count(), ss.count())
	}

	bad, _ := fix4_12_server(t, func(int) (int, string) { return 200, `{"answers":` })
	badSteps := []ComboStep{
		{Name: "Fix4-BadBody/clef", Base: bad.URL + "/v1/systemone", Key: "b", Model: "clef", Weight: 2},
		steps[1],
	}
	badOut := RunCombo(badSteps, 300*time.Millisecond)
	if badOut.Winner != fix4_12_secondary || badOut.Steps[0].OK || badOut.Steps[0].Detail != "invalid system one response" {
		t.Fatalf("malformed SO 200 must fail and fall back: %+v", badOut)
	}

	valid, _ := fix4_12_server(t, func(int) (int, string) { return 200, w11_reply("yes", 0.95)(1) })
	validSteps := []ComboStep{
		{Name: "Fix4-ValidBody/clef", Base: valid.URL + "/v1/systemone", Key: "v", Model: "clef", Weight: 2},
		steps[1],
	}
	validOut := RunCombo(validSteps, 300*time.Millisecond)
	if validOut.Winner != "Fix4-ValidBody/clef" || !validOut.Steps[0].OK {
		t.Fatalf("valid SO answer must stay OK: %+v", validOut)
	}
}
