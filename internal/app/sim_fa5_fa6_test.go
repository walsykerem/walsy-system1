package app

import (
	"bytes"
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
// decision path is reimplemented.

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

// FA6.7: keying folds exact polite/filler tokens (foldKeyState), so a lightly
// padded rephrase shares the cell; no embedding or paraphrase tier exists.
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
