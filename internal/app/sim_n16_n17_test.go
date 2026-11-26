package app

// W11 simulation for MANIFEST N16 (admit/negative TTL cache, degraded
// uncached, hot-key prewarm) and N17 (explicit state skeleton, prompt id/hash
// refs, hard per-request char cap). All package-level names use the w11_
// prefix so this file cannot collide with sibling simulation files.
// Every case drives production code against an httptest SO stub.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const w11_req = `{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`

type w11_so struct {
	mu     sync.Mutex
	posts  int
	bodies []string
	reply  func(int) string
}

func w11_reply(choice string, conf float64) func(int) string {
	return func(int) string {
		b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}}})
		return string(b)
	}
}

func w11_stub(t *testing.T, reply func(int) string) (*httptest.Server, *w11_so) {
	t.Helper()
	st := &w11_so{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		st.mu.Lock()
		st.posts++
		st.bodies = append(st.bodies, string(b))
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, st.reply(st.posts))
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func (s *w11_so) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func (s *w11_so) wire() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func w11_base(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "w11-n16-n17-key")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	routerReset()
}

func w11_attach(t *testing.T, srv *httptest.Server) {
	t.Helper()
	ep, cl := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = ep, cl })
}

func w11_key(d *decisionBridge) [32]byte {
	return d.cacheKey([]byte(w11_req), soCapability(), d.pin.generation())
}

// w11_primePin puts a fresh bridge in the steady state of an established
// session. route() bumps pin generation on its first successful admit; a cell
// computed under the previous generation is intentionally not reused.
func w11_primePin(d *decisionBridge) {
	d.pin.markSuccess(d.pin.current().Name)
}

func w11_cell(t *testing.T, d *decisionBridge, k [32]byte) *cacheCell {
	t.Helper()
	s := d.cache.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	el := s.idx[hex.EncodeToString(k[:])]
	if el == nil {
		t.Fatal("decision cell missing after a terminal result")
	}
	return el.Value.(*cacheCell)
}

func w11_setExpiry(t *testing.T, d *decisionBridge, k [32]byte, at time.Time) {
	t.Helper()
	s := d.cache.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	el := s.idx[hex.EncodeToString(k[:])]
	if el == nil {
		t.Fatal("decision cell missing before simulated expiry")
	}
	el.Value.(*cacheCell).expiresAt = at
}

// N16: admit cell lives ~24h, then re-asks SO.
func TestW11_N16TTLAdmit(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, w11_reply("yes", 0.95))
	w11_attach(t, srv)
	d := newDecisionBridge()
	w11_primePin(d)

	r1 := d.decide([]byte(w11_req))
	if r1.Decision != decPin || r1.Reason != "so-admit" {
		t.Fatalf("first decision = %+v, want pin/so-admit", r1)
	}
	if st.count() != 1 {
		t.Fatalf("first decision posts = %d, want 1", st.count())
	}
	k := w11_key(d)
	left := w11_cell(t, d, k).expiresAt.Sub(time.Now())
	if left < 23*time.Hour || left > 24*time.Hour {
		t.Fatalf("admit cell TTL = %v, want ~24h", left)
	}

	r2 := d.decide([]byte(w11_req))
	if r2.Decision != decPin || r2.Reason != "cache-hit" {
		t.Fatalf("second decision = %+v, want pin/cache-hit", r2)
	}
	if st.count() != 1 {
		t.Fatalf("cache hit posts = %d, want 1", st.count())
	}

	w11_setExpiry(t, d, k, time.Now().Add(-time.Second))
	r3 := d.decide([]byte(w11_req))
	if r3.Reason != "so-admit" || st.count() != 2 {
		t.Fatalf("expired admit not re-decided: reason=%s posts=%d", r3.Reason, st.count())
	}
}

// N16: terminal abstain cell lives ~1h, then re-asks SO.
func TestW11_N16TTLNegative(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, w11_reply("no", 0.95))
	w11_attach(t, srv)
	d := newDecisionBridge()

	r1 := d.decide([]byte(w11_req))
	if r1.Decision != decPass || r1.Reason != "so-no" || !r1.Abstained {
		t.Fatalf("first negative = %+v, want pass/so-no/abstain", r1)
	}
	if st.count() != 1 {
		t.Fatalf("first negative posts = %d, want 1", st.count())
	}
	k := w11_key(d)
	left := w11_cell(t, d, k).expiresAt.Sub(time.Now())
	if left < 30*time.Minute || left > time.Hour {
		t.Fatalf("negative cell TTL = %v, want ~1h", left)
	}

	r2 := d.decide([]byte(w11_req))
	if r2.Reason != "so-no" || !r2.Abstained || st.count() != 1 {
		t.Fatalf("negative cache hit = %+v posts=%d", r2, st.count())
	}

	w11_setExpiry(t, d, k, time.Now().Add(-time.Second))
	r3 := d.decide([]byte(w11_req))
	if r3.Reason != "so-no" || st.count() != 2 {
		t.Fatalf("expired negative not re-decided: reason=%s posts=%d", r3.Reason, st.count())
	}
}

// N16: an unusable SO answer abstains and is never cached.
func TestW11_N16DegradedUncached(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, func(int) string {
		return `{"answers":{"gate":{"type":"choice","choice":"out-of-set","confidence":0.9}}}`
	})
	w11_attach(t, srv)
	d := newDecisionBridge()

	for i := 1; i <= 2; i++ {
		r := d.decide([]byte(w11_req))
		if r.Decision != decPass || r.Reason != "so-degraded" {
			t.Fatalf("call %d = %+v, want pass/so-degraded", i, r)
		}
		if st.count() != i {
			t.Fatalf("call %d posts = %d, want %d (degraded must not cache)", i, st.count(), i)
		}
	}
	k := w11_key(d)
	s := d.cache.shard(k)
	s.mu.Lock()
	_, stored := s.idx[hex.EncodeToString(k[:])]
	s.mu.Unlock()
	if stored {
		t.Fatal("degraded outcome was written into the decision cache")
	}
}

// N16: a repeatedly used key must stay cached across its expiry boundary.
// Strict red test: there is no prewarm implementation in the production tree.
func TestW11_N16HotKeyPrewarm(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, w11_reply("yes", 0.95))
	w11_attach(t, srv)
	d := newDecisionBridge()
	w11_primePin(d)

	for i := 0; i < 3; i++ {
		r := d.decide([]byte(w11_req))
		if r.Decision != decPin {
			t.Fatalf("warmup %d = %+v", i, r)
		}
	}
	if st.count() != 1 {
		t.Fatalf("hot key warmup posts = %d, want 1", st.count())
	}
	k := w11_key(d)
	w11_setExpiry(t, d, k, time.Now().Add(40*time.Millisecond))
	time.Sleep(80 * time.Millisecond)

	r := d.decide([]byte(w11_req))
	if r.Reason != "cache-hit" {
		t.Fatalf("hot key lost across expiry: reason=%s posts=%d (no prewarm)", r.Reason, st.count())
	}
	if st.count() != 1 {
		t.Fatalf("prewarmed hot key re-asked SO: posts=%d", st.count())
	}
}

// N16: three identical in-scope frames through the real stdio relay. The
// first admit primes the cache on the current generation; all three frames
// reuse that cell. Strict expectation: 1 SO post, 3 forwarded frames.
func TestW11_N16TransportCacheHit(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, w11_reply("yes", 0.95))
	w11_attach(t, srv)

	frame := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"classify transport","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}` + "\n"
	reply := `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n"
	var clientOut, upstreamIn bytes.Buffer
	relayStdio(strings.NewReader(frame+frame+frame), &clientOut, &upstreamIn, strings.NewReader(reply+reply+reply))

	if st.count() != 1 {
		t.Fatalf("three identical relay frames caused %d SO posts, want 1 (first admit primes the cache)", st.count())
	}
	if n := strings.Count(upstreamIn.String(), `"method":"tools/call"`); n != 3 {
		t.Fatalf("relay forwarded %d frames, want 3", n)
	}
	if n := strings.Count(clientOut.String(), `"ok":true`); n != 3 {
		t.Fatalf("relay returned %d upstream replies, want 3", n)
	}
}

// N17: the real SO-bound skeleton carries named fields plus prompt id/hash
// refs, no body and no empty blocks.
func TestW11_N17SkeletonShapeAndWire(t *testing.T) {
	w11_base(t)
	empty, err := soStateFromEnvelope(stateEnvelope{})
	if err != nil || empty != "{}" {
		t.Fatalf("empty envelope skeleton = %q, %v; want {}", empty, err)
	}

	const body = "SECRET-PROMPT-BODY-DO-NOT-LEAK"
	if err := addPrompt("alpha", body); err != nil {
		t.Fatal(err)
	}
	srv, st := w11_stub(t, w11_reply("no", 0.9))
	w11_attach(t, srv)

	state := buildSOState(w11_req)
	if state == "" {
		t.Fatal("valid request rejected by skeleton build")
	}
	var skel map[string]any
	if err := json.Unmarshal([]byte(state), &skel); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "method", "need_label", "no_scratch", "safe", "reversible", "opt_in", "phase", "authority", "calibrated"} {
		if _, ok := skel[k]; !ok {
			t.Errorf("skeleton missing named field %q: %s", k, state)
		}
	}
	refs, ok := skel["prompts"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("skeleton prompt refs = %v", skel["prompts"])
	}
	ref, ok := refs[0].(map[string]any)
	if !ok || ref["id"] != "alpha" {
		t.Fatalf("skeleton prompt ref = %v", refs[0])
	}
	sum := sha256.Sum256([]byte(body))
	if ref["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("prompt digest = %v", ref["sha256"])
	}
	if _, bad := ref["body"]; bad {
		t.Fatalf("prompt ref carries body: %v", ref)
	}
	if strings.Contains(state, body) {
		t.Fatalf("skeleton leaked prompt body: %s", state)
	}

	d := askSOQOnce(state, soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}})
	if d.Choice != "no" {
		t.Fatalf("SO answer = %+v", d)
	}
	wires := st.wire()
	if len(wires) != 1 {
		t.Fatalf("SO posts = %d, want 1", len(wires))
	}
	var post map[string]any
	if err := json.Unmarshal([]byte(wires[0]), &post); err != nil {
		t.Fatal(err)
	}
	inner, _ := post["state"].(string)
	if strings.Contains(inner, body) || strings.Contains(wires[0], body) {
		t.Fatalf("SO POST leaked prompt body: %s", wires[0])
	}
	if !strings.Contains(inner, `"sha256"`) || !strings.Contains(inner, `"alpha"`) {
		t.Fatalf("SO POST lost prompt id/hash: %s", inner)
	}
}

// N17: prompt text stays local through the decision hub; an over-cap state is
// rejected before any SO POST (no truncation).
func TestW11_N17NoBodyAndCap(t *testing.T) {
	w11_base(t)
	const secret = "TOP-SECRET-PROMPT-TEXT-9f3a"
	if err := addPrompt("beta", secret); err != nil {
		t.Fatal(err)
	}
	srv, st := w11_stub(t, w11_reply("yes", 0.95))
	w11_attach(t, srv)
	d := newDecisionBridge()

	req := strings.Replace(w11_req, `"state":"classify this request"`, `"state":"classify this request","user_system_prompt":"`+secret+`"`, 1)
	r := d.decide([]byte(req))
	if r.Decision != decPin {
		t.Fatalf("in-scope request = %+v, want pin", r)
	}
	wires := st.wire()
	if len(wires) != 1 {
		t.Fatalf("SO posts = %d, want 1", len(wires))
	}
	if strings.Contains(wires[0], secret) {
		t.Fatalf("decision hub leaked prompt body to SO: %s", wires[0])
	}
	var post map[string]any
	if err := json.Unmarshal([]byte(wires[0]), &post); err != nil {
		t.Fatal(err)
	}
	inner, _ := post["state"].(string)
	if !strings.Contains(inner, `"beta"`) || !strings.Contains(inner, `"sha256"`) {
		t.Fatalf("SO state lost prompt refs: %s", inner)
	}

	posts := st.count()
	huge := strings.Repeat("x", maxSOStateChars+1)
	overReq := strings.Replace(w11_req, `"state":"classify this request"`, `"state":"classify `+huge+`"`, 1)
	if over := buildSOState(overReq); over != "" {
		t.Fatalf("over-cap state built (%d chars)", len(over))
	}
	r2 := d.decide([]byte(overReq))
	if r2.Decision != decPass {
		t.Fatalf("over-cap decision = %+v, want pass", r2)
	}
	if st.count() != posts {
		t.Fatalf("over-cap state reached SO: posts %d -> %d", posts, st.count())
	}
}
