package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Simulation of MANIFEST YD2 (engine generation guard: a mid-flight engine
// change must never put the old engine's answer in the cache; invalidation is
// engine identity, not TTL) and YD8 (truncated state and broken probability
// distribution must produce a typed error, not a fallthrough; 1 MiB response
// cap; 0.01 normalization tolerance). Every case drives production code
// (decide/decideFrame/askSOQOnce/route/relayStdio) against an httptest SO stub.

const w35_req = `{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`

type w35_stub struct {
	mu     sync.Mutex
	posts  int
	bodies []string
	reply  func(int) string
}

func w35_replyChoice(choice string, conf float64) func(int) string {
	return func(int) string {
		b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}}})
		return string(b)
	}
}

func w35_stubServer(t *testing.T, reply func(int) string) (*httptest.Server, *w35_stub) {
	t.Helper()
	st := &w35_stub{reply: reply}
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

func (s *w35_stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func w35_base(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-w35-test-key")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	routerReset()
}

func w35_attach(t *testing.T, srv *httptest.Server) {
	t.Helper()
	ep, cl := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = ep, cl })
}

const w35_frame = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"classify transport","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}` + "\n"

func w35_cellExists(d *decisionBridge, key [32]byte) bool {
	s := d.cache.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.idx[hex.EncodeToString(key[:])]
	return ok
}

// TestSimYD2MidflightSwapNeverCachesStaleAnswer: the generation guard in
// decide() must drop the computed answer when rotateOnFailure() happens
// between generation read and cache write. The old generation key stays empty;
// the new generation re-asks and caches normally.
func TestSimYD2MidflightSwapNeverCachesStaleAnswer(t *testing.T) {
	w35_base(t)
	var d *decisionBridge
	var swapped atomic.Bool
	srv, st := w35_stubServer(t, func(post int) string {
		if post == 1 && swapped.CompareAndSwap(false, true) {
			d.pin.rotateOnFailure() // engine identity changes mid-flight
		}
		return w35_replyChoice("yes", 0.95)(post)
	})
	w35_attach(t, srv)
	d = newDecisionBridge()

	genBefore := d.pin.generation()
	keyOld := d.cacheKey([]byte(w35_req), soCapability(), genBefore)

	r1 := d.decide([]byte(w35_req))
	if r1.Decision != decPin || r1.Reason != "so-admit" {
		t.Fatalf("mid-flight decision = %+v, want pin/so-admit", r1)
	}
	genAfter := d.pin.generation()
	if genAfter <= genBefore {
		t.Fatalf("generation did not change mid-flight: before=%d after=%d", genBefore, genAfter)
	}
	if w35_cellExists(d, keyOld) {
		t.Fatalf("GAP: stale mid-flight answer cached under old engine key gen=%d", genBefore)
	}
	if st.count() != 1 {
		t.Fatalf("first call posts = %d, want 1", st.count())
	}

	keyNew := d.cacheKey([]byte(w35_req), soCapability(), genAfter)
	r2 := d.decide([]byte(w35_req))
	if r2.Reason != "so-admit" || st.count() != 2 {
		t.Fatalf("new engine did not re-ask: decision=%+v posts=%d", r2, st.count())
	}
	if !w35_cellExists(d, keyNew) {
		t.Fatalf("new engine answer was not cached under gen=%d", genAfter)
	}

	r3 := d.decide([]byte(w35_req))
	if r3.Reason != "cache-hit" || st.count() != 2 {
		t.Fatalf("new engine cache unusable: decision=%+v posts=%d", r3, st.count())
	}
}

// TestSimYD2HandlerGenerationRebind: through the real decideFrame handler, a
// pin swap invalidates the previous engine's cell immediately - no TTL wait.
func TestSimYD2HandlerGenerationRebind(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, w35_replyChoice("yes", 0.95))
	w35_attach(t, srv)
	d := newDecisionBridge()
	// First admit pins an engine (gen++) and its answer lands under the old
	// key; stabilize identity before measuring the cache hit.
	d.pin.markSuccess(d.pin.current().Name)
	gen := d.pin.generation()
	f, err := decodeFrame([]byte(w35_frame))
	if err != nil {
		t.Fatal(err)
	}
	keyOld := d.cacheKey(unwrapParams([]byte(w35_frame), f), soCapability(), gen)
	frame := []byte(w35_frame)

	if r := d.decideFrame(frame); r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("frame 1 = %+v, want pin/so-admit", r)
	}
	if r := d.decideFrame(frame); r.Reason != "cache-hit" || st.count() != 1 {
		t.Fatalf("frame 2 = %+v posts=%d, want cache-hit/1", r, st.count())
	}

	d.pin.rotateOnFailure()
	if r := d.decideFrame(frame); r.Reason != "so-admit" || st.count() != 2 {
		t.Fatalf("after engine swap = %+v posts=%d, want re-ask", r, st.count())
	}
	if d.pin.generation() <= gen || d.cacheKey(unwrapParams([]byte(w35_frame), f), soCapability(), d.pin.generation()) == keyOld {
		t.Fatal("engine identity did not rebind the cache key")
	}
}

// TestSimYD2TransportRestartReasksOracle: two identical frames share one SO
// decision; a fresh bridge (process restart, RAM-only pin+cache) re-asks.
func TestSimYD2TransportRestartReasksOracle(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, w35_replyChoice("yes", 0.95))
	w35_attach(t, srv)

	reply := `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n"
	var clientOut, upstreamIn bytes.Buffer
	// First admit primes the cache on the current generation; frames 2 and 3
	// must reuse that cell with no extra SO POST.
	relayStdio(strings.NewReader(w35_frame+w35_frame+w35_frame), &clientOut, &upstreamIn, strings.NewReader(reply))
	if st.count() != 1 {
		t.Fatalf("warm process posts = %d, want 1 (first admit primes the cache)", st.count())
	}
	if n := strings.Count(upstreamIn.String(), `"method":"tools/call"`); n != 3 {
		t.Fatalf("relay forwarded %d frames, want 3", n)
	}
	if !strings.Contains(clientOut.String(), `"ok":true`) {
		t.Fatalf("upstream reply not returned: %q", clientOut.String())
	}

	// Simulate a binary restart: new bridge, new RAM generation, new cache.
	d := newDecisionBridge()
	r := d.decide([]byte(w35_req))
	if r.Reason != "so-admit" || st.count() != 2 {
		t.Fatalf("post-restart decision = %+v posts=%d, want re-ask", r, st.count())
	}
}

func w35_reply(probabilities map[string]float64) string {
	a := map[string]any{"type": "choice", "choice": "yes", "confidence": 0.95}
	if probabilities != nil {
		a["probabilities"] = probabilities
	}
	b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": a}})
	return string(b)
}

// TestSimYD8TruncatedStateTypedError: over-cap state must be rejected before
// any SO POST and carry the typed input_truncated reason (JS RouterError).
func TestSimYD8TruncatedStateTypedError(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, w35_replyChoice("yes", 0.95))
	w35_attach(t, srv)
	d := newDecisionBridge()

	huge := strings.Repeat("x", maxSOStateChars+1)
	overReq := strings.Replace(w35_req, `"state":"classify this request"`, `"state":"classify `+huge+`"`, 1)
	r := d.decide([]byte(overReq))

	if r.Decision != decPass {
		t.Fatalf("over-cap decision = %+v, want pass", r)
	}
	if st.count() != 0 {
		t.Fatalf("over-cap state reached SO: posts=%d", st.count())
	}
	if r.Reason != "input_truncated" {
		t.Errorf("GAP: over-cap reason = %q, want typed input_truncated (no fallthrough reason)", r.Reason)
	}
}

// TestSimYD8TimeoutTypedError: an SO deadline must abstain, set TimedOut,
// stay uncached and carry the typed timeout reason.
func TestSimYD8TimeoutTypedError(t *testing.T) {
	w35_base(t)
	oldTimeout := soTimeout
	soTimeout = 40 * time.Millisecond
	t.Cleanup(func() { soTimeout = oldTimeout })

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, w35_replyChoice("yes", 0.95)(1))
	}))
	t.Cleanup(srv.Close)
	w35_attach(t, srv)
	d := newDecisionBridge()

	r1 := d.decide([]byte(w35_req))
	if !r1.LastDecision.TimedOut {
		t.Fatalf("timeout not observed in typed decision: %+v", r1.LastDecision)
	}
	if r1.Decision != decPass {
		t.Fatalf("timed-out decision = %+v, want pass", r1)
	}
	if r1.Reason != "timeout" {
		t.Errorf("GAP: timeout reason = %q, want typed timeout (classify maps deadline to transient)", r1.Reason)
	}
	if posts.Load() != 1 {
		t.Fatalf("timeout posts = %d, want 1", posts.Load())
	}

	r2 := d.decide([]byte(w35_req))
	if posts.Load() != 2 || r2.Decision != decPass {
		t.Fatalf("timeout outcome was cached: decision=%+v posts=%d", r2, posts.Load())
	}
}

// TestSimYD8ResponseCapNoFallthrough: an SO response above 1 MiB must not be
// accepted from its in-cap prefix, and must carry a typed malformed reason.
func TestSimYD8ResponseCapNoFallthrough(t *testing.T) {
	w35_base(t)
	prefix := w35_replyChoice("yes", 0.95)(1)
	body := prefix + strings.Repeat(" ", (1<<20)+4096) +
		`{"answers":{"gate":{"type":"choice","choice":"no","confidence":1}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	w35_attach(t, srv)
	d := newDecisionBridge()

	r := d.decide([]byte(w35_req))
	if r.LastDecision.Choice != "" {
		t.Errorf("GAP: oversize response produced choice %q from the in-cap prefix", r.LastDecision.Choice)
	}
	if r.Decision != decPass {
		t.Errorf("GAP: oversize decision = %+v, want pass", r)
	}
	if r.Reason != "malformed_response" {
		t.Errorf("GAP: oversize reason = %q, want typed malformed_response", r.Reason)
	}
}

// TestSimYD8InvalidProbabilitiesTypedError: a probability distribution whose
// sum is far from 1 must be rejected, not admitted on confidence alone.
func TestSimYD8InvalidProbabilitiesTypedError(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, func(int) string {
		return w35_reply(map[string]float64{"yes": 0.9, "no": 0.9})
	})
	w35_attach(t, srv)
	d := newDecisionBridge()

	r := d.decide([]byte(w35_req))
	if r.Decision != decPass || r.Reason != "invalid_probabilities" {
		t.Errorf("GAP: broken distribution = %+v, want pass/invalid_probabilities (conf=%v sum=1.8)",
			r, r.LastDecision.Conf)
	}
	if st.count() != 1 {
		t.Fatalf("posts = %d, want 1", st.count())
	}
}

// TestSimYD8NormalizeTolerance: a distribution within 0.01 of 1 must be
// normalized and produce a comparable margin; current code ignores it.
func TestSimYD8NormalizeTolerance(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, func(int) string {
		return w35_reply(map[string]float64{"yes": 0.60, "no": 0.39}) // sum 0.99
	})
	w35_attach(t, srv)
	d := newDecisionBridge()

	r := d.decide([]byte(w35_req))
	ld := r.LastDecision
	if !ld.MarginKnown {
		t.Errorf("GAP: margin unknown for within-tolerance distribution probs=%v (validSOAnswer requires type=choice, answerMargin only runs for type=score)", ld.Probabilities)
	} else if ld.Margin < 0.20 {
		t.Errorf("margin = %v, want normalized ~0.21", ld.Margin)
	}
	if st.count() != 1 {
		t.Fatalf("posts = %d, want 1", st.count())
	}
}

// TestSimYD8TransportFailOpenOnMalformedResponse: bad SO JSON must fail open
// through the real stdio relay - both frames forwarded, no cached decision.
func TestSimYD8TransportFailOpenOnMalformedResponse(t *testing.T) {
	w35_base(t)
	srv, st := w35_stubServer(t, func(int) string { return `{"answers":` })
	w35_attach(t, srv)

	reply := `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}` + "\n"
	var clientOut, upstreamIn bytes.Buffer
	relayStdio(strings.NewReader(w35_frame+w35_frame), &clientOut, &upstreamIn, strings.NewReader(reply))

	if st.count() != 2 {
		t.Fatalf("malformed SO response posts = %d, want 2 (never cached)", st.count())
	}
	if n := strings.Count(upstreamIn.String(), `"method":"tools/call"`); n != 2 {
		t.Fatalf("relay forwarded %d frames, want 2", n)
	}
	if !strings.Contains(clientOut.String(), `"ok":true`) {
		t.Fatalf("upstream reply not returned: %q", clientOut.String())
	}
}
