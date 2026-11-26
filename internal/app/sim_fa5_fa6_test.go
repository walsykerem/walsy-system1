package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// W27 simulation of MANIFEST FA5 (local preflight shadow) and FA6 (decision
// cache). Every case drives production code: the rule tree, route, bridge,
// HTTP handler and the real SO HTTP client against an httptest oracle. No
// decision path is reimplemented. The only probe that mirrors key derivation
// (w27_VersionKey) is first checked against production gateKeyBytes.

const w27_Req = `{"state":"classify w27 simulation","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`

type w27_SO struct {
	mu     sync.Mutex
	posts  int
	bodies []string
	reply  func(int) string
}

func w27_Reply(choice string, conf float64) func(int) string {
	return func(int) string {
		b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}}})
		return string(b)
	}
}

func w27_Stub(t *testing.T, reply func(int) string) (*httptest.Server, *w27_SO) {
	t.Helper()
	st := &w27_SO{reply: reply}
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

func (s *w27_SO) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func w27_Base(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	routerReset()
	t.Cleanup(routerReset)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "w27-sim-key")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
}

func w27_Attach(t *testing.T, srv *httptest.Server) {
	t.Helper()
	ep, cl := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = ep, cl })
}

func w27_Capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	b := &bytes.Buffer{}
	old := metricOut
	metricOut = b
	t.Cleanup(func() { metricOut = old })
	return b
}

func w27_Audit(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func w27_Key(d *decisionBridge, request []byte) [32]byte {
	return d.cacheKey(request, soCapability(), d.pin.generation())
}

func w27_Cell(t *testing.T, d *decisionBridge, k [32]byte) *cacheCell {
	t.Helper()
	s := d.cache.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	el := s.idx[hex.EncodeToString(k[:])]
	if el == nil {
		t.Fatalf("cache cell missing: key=%s", hex.EncodeToString(k[:]))
	}
	return el.Value.(*cacheCell)
}

func w27_Expire(t *testing.T, d *decisionBridge, k [32]byte, at time.Time) {
	t.Helper()
	s := d.cache.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	el := s.idx[hex.EncodeToString(k[:])]
	if el == nil {
		t.Fatal("cache cell missing before simulated expiry")
	}
	el.Value.(*cacheCell).expiresAt = at
}

// w27_VersionKey mirrors gateKeyBytes with an explicit policy version. Used only
// after equality with the shipped gateKeyBytes is asserted.
func w27_VersionKey(request []byte, cap capabilityConfig, version string) [32]byte {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(request))
	dec.UseNumber()
	if dec.Decode(&obj) != nil {
		obj = nil
	}
	if obj != nil {
		for _, k := range []string{"id", "jsonrpc"} {
			delete(obj, k)
		}
		foldKeyState(obj)
	}
	b, _ := json.Marshal(obj)
	h := sha256.New()
	_, _ = h.Write([]byte(canonicalState(string(b))))
	for _, name := range activeQuestionNames(string(b)) {
		writeField(h, name)
	}
	c, _ := json.Marshal(cap)
	writeField(h, string(c))
	writeField(h, keyRubricVersion)
	writeField(h, version)
	writeField(h, keyModel)
	writeField(h, keyConfRule)
	writeField(h, registryVersion)
	writeField(h, promptSetHash())
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// FA5.1: the rule tree is local and byte-stable; it never contacts SO.
func TestSimFA5RuleTree(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, w27_Reply("yes", 0.95))
	w27_Attach(t, srv)

	base := stateEnvelope{
		State: "classify this request", Method: "tools/call",
		NeedLabel: true, NoScratch: true, Safe: true, Reversible: true,
		OptIn: true, Phase: "before-model", Authority: true, Calibrated: true,
	}
	cases := []struct {
		name   string
		change func(*stateEnvelope)
		reason string
		ambig  bool
	}{
		{"out-of-scope", func(e *stateEnvelope) { e.Phase = "" }, "layer0-scope", false},
		{"static", func(e *stateEnvelope) { e.State = "ping" }, "layer0-static", false},
		{"shape", func(e *stateEnvelope) { e.State = "summarize the document" }, "layer0-shape", false},
		{"registry-local", func(e *stateEnvelope) { e.Item = "reasoning.priority" }, "registry-local", false},
		{"so-pending", func(e *stateEnvelope) {}, "so-pending", false},
		{"ambiguous", func(e *stateEnvelope) { e.Safe = false }, "so-pending", true},
	}
	for _, tc := range cases {
		e := base
		tc.change(&e)
		start := time.Now()
		got := preflight(e)
		elapsed := time.Since(start)
		if got.Reason != tc.reason || got.Ambiguous != tc.ambig {
			t.Errorf("%s: preflight = %+v, want reason=%s ambiguous=%v", tc.name, got, tc.reason, tc.ambig)
		}
		if elapsed > 5*time.Millisecond {
			t.Errorf("%s: rule tree took %v, want local microseconds", tc.name, elapsed)
		}
	}
	if st.count() != 0 {
		t.Fatalf("rule tree caused %d SO posts, want 0", st.count())
	}
}

// FA5.2: a request the shadow calls ambiguous must still reach SO.
func TestSimFA5AmbiguousToSO(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, w27_Reply("yes", 0.95))
	w27_Attach(t, srv)

	state := `{"state":"classify this request","method":"tools/call","need_label":true,"no_scratch":true,"safe":false,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`
	p := preflightFromState(state)
	if p.Reason != "so-pending" || !p.Ambiguous {
		t.Fatalf("preflight = %+v, want so-pending/ambiguous", p)
	}
	r := route(state, newPin(nil))
	if st.count() != 1 {
		t.Fatalf("ambiguous request did not reach SO: route=%+v posts=%d", r, st.count())
	}
}

// FA5.3: the shadow is advisory only; real admission comes from SO, the ring
// accumulates reason pairs, and metric/audit are source-tagged.
func TestSimFA5AdvisoryShadow(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, func(i int) string {
		if i == 1 {
			return w27_Reply("yes", 0.95)(i)
		}
		return w27_Reply("no", 0.95)(i)
	})
	w27_Attach(t, srv)
	buf := w27_Capture(t)
	d := newDecisionBridge()

	r1 := d.decide([]byte(w27_Req))
	if r1.Decision != decPin || r1.Reason != "so-admit" {
		t.Fatalf("admit decision = %+v, want pin/so-admit", r1)
	}
	neg := strings.Replace(w27_Req, "classify w27 simulation", "classify w27 negative", 1)
	r2 := d.decide([]byte(neg))
	if r2.Decision != decPass || r2.Reason != "so-no" || !r2.Abstained {
		t.Fatalf("negative decision = %+v, want pass/so-no", r2)
	}
	if d.preflight.counts["so-pending|so-admit"] == 0 || d.preflight.counts["so-pending|so-no"] == 0 {
		t.Fatalf("calibration ring did not record both pairs: %v", d.preflight.counts)
	}

	out := strings.Replace(w27_Req, `"method":"tools/call"`, `"method":"notification"`, 1)
	r3 := d.decide([]byte(out))
	if r3.Decision != decPass || r3.Reason != "layer0-scope" {
		t.Fatalf("out-of-scope decision = %+v, want pass/layer0-scope", r3)
	}
	if st.count() != 2 {
		t.Fatalf("SO posts = %d, want 2 (out-of-scope must not ask)", st.count())
	}

	m := buf.String()
	if !strings.Contains(m, `"source":"preflight"`) || !strings.Contains(m, `"route":"preflight"`) {
		t.Fatalf("shadow metric missing source/route: %s", m)
	}
	if strings.Contains(m, `"confidence"`) {
		t.Fatalf("raw confidence leaked into metric: %s", m)
	}
	if n := strings.Count(string(w27_Audit(t)), `"source":"preflight"`); n < 3 {
		t.Fatalf("shadow audit lines = %d, want >=3", n)
	}

	ring := newPreflightCalibration()
	for i := 0; i < 300; i++ {
		ring.add("so-pending", "r"+strconv.Itoa(i))
	}
	if ring.n != 256 || len(ring.counts) > 256 {
		t.Fatalf("calibration ring not bounded: n=%d counts=%d", ring.n, len(ring.counts))
	}
}

// FA6.1: canonical SHA-256 [32]byte key strips volatile fields, keeps meaning.
func TestSimFA6CanonicalKey(t *testing.T) {
	w27_Base(t)
	cap := soCapability()
	a := []byte(`{"params":{"state":"classify payload","session_id":"s1","timestamp":1}}`)
	b := []byte(`{"params":{"state":"classify payload","sessionId":"s2","time":2}}`)
	ka, kb := gateKeyBytes(a, cap), gateKeyBytes(b, cap)
	var digest [32]byte = ka
	_ = digest
	if ka != kb {
		t.Fatal("volatile fields changed the canonical key")
	}
	sem := []byte(`{"params":{"state":"rank payload","session_id":"s1","timestamp":1}}`)
	if gateKeyBytes(sem, cap) == ka {
		t.Fatal("semantic change did not rebind the canonical key")
	}
	if gateKey(a, cap) != hex.EncodeToString(ka[:]) {
		t.Fatal("gateKey is not hex of gateKeyBytes")
	}
}

// FA6.2: admit cells live ~24h, expire lazily, then re-ask SO.
func TestSimFA6TTLAdmit(t *testing.T) {
	w27_Base(t)

	// Cold engine: the first admit must be reusable. Observe the real identity
	// change caused by the first successful oracle answer.
	srv0, st0 := w27_Stub(t, w27_Reply("yes", 0.95))
	w27_Attach(t, srv0)
	cold := newDecisionBridge()
	gen0 := cold.pin.generation()
	if r := cold.decide([]byte(w27_Req)); r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("first decision = %+v", r)
	}
	gen1 := cold.pin.generation()
	if r := cold.decide([]byte(w27_Req)); r.Reason != "cache-hit" || st0.count() != 1 {
		t.Errorf("FA6 admit cache BROKEN on cold pin: second reason=%s posts=%d generation %d->%d: markSuccess bumps identity after the admit cell is computed", r.Reason, st0.count(), gen0, gen1)
	}

	// Warm engine: identity is stable, so TTL, hit and lazy expiry are isolated.
	srv, st := w27_Stub(t, w27_Reply("yes", 0.95))
	w27_Attach(t, srv)
	d := newDecisionBridge()
	d.pin.markSuccess(d.pin.current().Name)
	if r := d.decide([]byte(w27_Req)); r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("warm first decision = %+v", r)
	}
	k := w27_Key(d, []byte(w27_Req))
	left := w27_Cell(t, d, k).expiresAt.Sub(time.Now())
	if left < 23*time.Hour || left > 24*time.Hour {
		t.Fatalf("admit TTL = %v, want ~24h", left)
	}
	if r := d.decide([]byte(w27_Req)); r.Reason != "cache-hit" || st.count() != 1 {
		t.Fatalf("cache hit = %+v posts=%d", r, st.count())
	}
	w27_Expire(t, d, k, time.Now().Add(-time.Second))
	if r := d.decide([]byte(w27_Req)); r.Reason != "so-admit" || st.count() != 2 {
		t.Fatalf("expired admit not re-asked: %+v posts=%d", r, st.count())
	}
}

// FA6.3: terminal abstains live ~1h and then re-ask SO.
func TestSimFA6TTLNegative(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, w27_Reply("no", 0.95))
	w27_Attach(t, srv)
	d := newDecisionBridge()

	if r := d.decide([]byte(w27_Req)); r.Decision != decPass || r.Reason != "so-no" || !r.Abstained {
		t.Fatalf("first negative = %+v", r)
	}
	k := w27_Key(d, []byte(w27_Req))
	left := w27_Cell(t, d, k).expiresAt.Sub(time.Now())
	if left < 30*time.Minute || left > time.Hour {
		t.Fatalf("negative TTL = %v, want ~1h", left)
	}
	if r := d.decide([]byte(w27_Req)); r.Reason != "so-no" || st.count() != 1 {
		t.Fatalf("negative cache hit = %+v posts=%d", r, st.count())
	}
	w27_Expire(t, d, k, time.Now().Add(-time.Second))
	if r := d.decide([]byte(w27_Req)); r.Reason != "so-no" || st.count() != 2 {
		t.Fatalf("expired negative not re-asked: %+v posts=%d", r, st.count())
	}
}

// FA6.4: each shard runs its own LRU; a touched entry is not the eviction victim.
func TestSimFA6LRUShard(t *testing.T) {
	w27_Base(t)
	c := newShardedCache()
	cap := soCapability()
	first := gateKeyBytes([]byte(`{"params":{"state":"classify lru 0"}}`), cap)
	target := int(first[0] & 7)
	keys := [][32]byte{first}
	for i := 1; i < 500 && len(keys) < 3; i++ {
		k := gateKeyBytes([]byte(`{"params":{"state":"classify lru `+strconv.Itoa(i)+`"}}`), cap)
		if int(k[0]&7) == target {
			keys = append(keys, k)
		}
	}
	if len(keys) != 3 {
		t.Fatal("could not build three keys in one shard")
	}
	s := c.shard(keys[0])
	s.cap = 2
	winner := cachedDecision{Choice: "yes", ConfBucket: "1.0", Reason: "margin=1"}
	loser := cachedDecision{Abstain: true, Reason: "so-no", ConfBucket: "0.9"}
	c.store(keys[0], winner)
	c.store(keys[1], loser)
	if _, ok := c.lookup(keys[0]); !ok {
		t.Fatal("entry vanished before eviction")
	}
	c.store(keys[2], winner)
	if _, ok := c.lookup(keys[1]); ok {
		t.Fatal("LRU evicted the touched entry instead of the oldest")
	}
	for _, k := range []struct {
		key  [32]byte
		want string
	}{{keys[0], "margin=1"}, {keys[2], "margin=1"}} {
		d, ok := c.lookup(k.key)
		if !ok || d.Reason != k.want {
			t.Fatalf("survivor %s = %+v ok=%v", hex.EncodeToString(k.key[:]), d, ok)
		}
	}
}

// FA6.5: policy_version is part of the key namespace; a bump misses old cells
// without any explicit flush.
func TestSimFA6PolicyVersionNamespace(t *testing.T) {
	w27_Base(t)
	cap := soCapability()
	req := []byte(w27_Req)
	prod := gateKeyBytes(req, cap)
	if probe := w27_VersionKey(req, cap, keyPolicyVersion); probe != prod {
		t.Fatalf("version probe does not match production key: probe=%x prod=%x", probe, prod)
	}
	c := newShardedCache()
	c.store(prod, cachedDecision{Choice: "yes", ConfBucket: "1.0", Reason: "margin=1"})
	if _, ok := c.lookup(prod); !ok {
		t.Fatal("shipped cell missing before bump")
	}
	bumped := w27_VersionKey(req, cap, "2")
	if bumped == prod {
		t.Fatal("policy_version does not rebind the key")
	}
	if _, ok := c.lookup(bumped); ok {
		t.Fatal("old cell served after policy_version bump; flush would be required")
	}
}

// FA6.6: hash-only precedent. Requests differing only in volatile/id fields
// share one cell; the cell stores only a hex digest and a typed decision.
func TestSimFA6HashOnlyCache(t *testing.T) {
	w27_Base(t)
	// A negative answer is a terminal cell on a cold engine, so this test
	// isolates identity stripping from the warm-pin admit cell.
	srv, st := w27_Stub(t, w27_Reply("no", 0.95))
	w27_Attach(t, srv)
	d := newDecisionBridge()

	a := strings.Replace(w27_Req, `{`, `{"id":1,"jsonrpc":"2.0",`, 1)
	b := strings.Replace(a, `"id":1`, `"id":99`, 1)
	if r := d.decide([]byte(a)); r.Reason != "so-no" {
		t.Fatalf("first decision = %+v", r)
	}
	if r := d.decide([]byte(b)); r.Reason != "so-no" || st.count() != 1 {
		t.Fatalf("id-only change did not hit cache: %+v posts=%d", r, st.count())
	}
	k := w27_Key(d, []byte(a))
	cell := w27_Cell(t, d, k)
	if cell.key != hex.EncodeToString(k[:]) {
		t.Fatalf("cell key is not the hex digest: %s", cell.key)
	}
	typ := reflect.TypeOf(cell.dec)
	allowed := map[string]bool{"Choice": true, "ConfBucket": true, "Reason": true, "Abstain": true, "Label": true, "Steer": true}
	if typ.NumField() != len(allowed) {
		t.Fatalf("cachedDecision has %d fields, want only typed decision/bucket", typ.NumField())
	}
	for i := 0; i < typ.NumField(); i++ {
		if !allowed[typ.Field(i).Name] {
			t.Fatalf("cachedDecision stores raw field %s", typ.Field(i).Name)
		}
	}
}

// FA6.7: exact-only keying has no semantic tier. Two equivalent phrasings must
// share a cached decision for this item to WORK; today each phrase pays SO.
func TestSimFA6SemanticThreshold(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, w27_Reply("no", 0.95))
	w27_Attach(t, srv)
	d := newDecisionBridge()

	a := w27_Req
	b := strings.Replace(a, "classify w27 simulation", "please classify this same w27 simulation", 1)
	if r := d.decide([]byte(a)); r.Decision != decPass || r.Reason != "so-no" {
		t.Fatalf("first decision = %+v", r)
	}
	r := d.decide([]byte(b))
	if r.Reason != "so-no" || st.count() != 1 {
		t.Fatalf("semantic equivalent did not hit cache: reason=%s posts=%d (exact canonical key only)", r.Reason, st.count())
	}
}

// FA5+FA6 through the real Streamable HTTP handler: two identical in-scope
// frames share one oracle answer and both emit advisory shadow records.
func TestSimFA6HTTPTransportAdvisory(t *testing.T) {
	w27_Base(t)
	srv, st := w27_Stub(t, w27_Reply("no", 0.95))
	w27_Attach(t, srv)
	buf := w27_Capture(t)

	ts := httptest.NewServer(mcpHTTPHandler())
	t.Cleanup(ts.Close)
	frame := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"probe","arguments":{},"state":"classify w27 transport","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}`
	for i := 1; i <= 2; i++ {
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(frame))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":7`) {
			t.Fatalf("request %d: status=%d body=%s", i, resp.StatusCode, body)
		}
	}
	if st.count() != 1 {
		t.Fatalf("two HTTP frames caused %d SO posts, want 1", st.count())
	}
	if n := strings.Count(buf.String(), `"source":"preflight"`); n != 2 {
		t.Fatalf("advisory shadow metrics = %d, want 2: %s", n, buf.String())
	}
	if n := strings.Count(string(w27_Audit(t)), `"source":"preflight"`); n != 2 {
		t.Fatalf("advisory shadow audit lines = %d, want 2", n)
	}
}
