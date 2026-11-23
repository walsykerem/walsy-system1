package main

// Hermetic runtime proof for MANIFEST FA7 and FA8.
// MANIFEST verbatim:
//   FA7: "Singleflight `map[key]*call{done chan}` - ayni key icin tek SO POST,
//         bekleyenler ayni cevabi alir."
//   FA8: "Sticky pin + hysteresis >=0.8 + stabil `session_id`/`prompt_cache_key`;
//         strict hit = 0 SO call; upstream prefix/KV cache sicak."
// No real SO call, key read, process restart or production edit. Real bridge,
// real route, real transport and a loopback httptest SO stub are exercised.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const w28_caps = `"need_label":true,"no_scratch":true,"safe":true,` +
	`"reversible":true,"authority":true,"calibrated":true,` +
	`"opt_in":true,"phase":"before-model"`

type w28_stub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []string
	answer  string
	enter   chan int
	block   chan struct{}
	blockOn int
}

func w28_newStub(answer string, blockOn int) *w28_stub {
	s := &w28_stub{answer: answer, enter: make(chan int, 64), blockOn: blockOn}
	if blockOn > 0 {
		s.block = make(chan struct{})
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *w28_stub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.bodies = append(s.bodies, string(body))
	n := len(s.bodies)
	answer := s.answer
	s.mu.Unlock()
	select {
	case s.enter <- n:
	default:
	}
	if s.block != nil && n == s.blockOn {
		<-s.block
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, answer)
}

func (s *w28_stub) release() {
	if s.block != nil {
		close(s.block)
	}
}

func (s *w28_stub) setAnswer(answer string) {
	s.mu.Lock()
	s.answer = answer
	s.mu.Unlock()
}

func (s *w28_stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *w28_stub) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func w28_env(t *testing.T, answer string, blockOn int) (*decisionBridge, *w28_stub) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("WALSY_ROUTER_DENY", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("EVREN_API_KEY", "sim-fa78-only-key")
	routerReset()
	stub := w28_newStub(answer, blockOn)
	old := soEndpoint
	soEndpoint = stub.srv.URL
	t.Cleanup(func() {
		soEndpoint = old
		stub.srv.Close()
		routerReset()
	})
	return newDecisionBridge(), stub
}

func w28_answer(conf float64) string {
	b, _ := json.Marshal(map[string]any{
		"model": "clef",
		"answers": map[string]any{"gate": map[string]any{
			"type":          "choice",
			"choice":        "yes",
			"confidence":    conf,
			"probabilities": map[string]float64{"yes": conf, "no": 1 - conf},
		}},
	})
	return string(b)
}

func w28_frame(id int, state, extra string) []byte {
	s, _ := json.Marshal(state)
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"initialize","params":{"state":` + string(s) + `,` + w28_caps + extra + `}}`)
}

func w28_signature(r routeResult) string {
	return fmt.Sprintf("decision=%s reason=%s covered=%v abstained=%v bucket=%s label=%s last=%s/%v",
		r.Decision, r.Reason, r.Covered, r.Abstained, r.ConfBucket, r.Label,
		r.LastDecision.Choice, r.LastDecision.Conf)
}

func w28_waitEnter(t *testing.T, stub *w28_stub) int {
	t.Helper()
	select {
	case n := <-stub.enter:
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("FA7: no SO POST reached the loopback stub")
		return -1
	}
}

func w28_waitAll(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("FA7: timed out waiting for %s", what)
	}
}

func w28_receipts(t *testing.T) []receipt {
	t.Helper()
	path, err := receiptPath()
	if err != nil {
		t.Fatalf("receiptPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read receipt chain: %v", err)
	}
	var out []receipt
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad receipt line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func w28_checkReceiptChain(t *testing.T) {
	t.Helper()
	prev := ""
	for i, r := range w28_receipts(t) {
		if r.Seq != i+1 {
			t.Errorf("FA7 BROKEN: receipt seq at position %d = %d, want %d", i, r.Seq, i+1)
		}
		if r.Prev != prev {
			t.Errorf("FA7 BROKEN: receipt %d prev=%q, want %q", i, r.Prev, prev)
		}
		body, err := canonicalJSON(r)
		if err != nil {
			t.Fatalf("canonical receipt: %v", err)
		}
		raw, err := hex.DecodeString(r.Prev)
		if err != nil {
			t.Fatalf("receipt prev is not hex: %v", err)
		}
		h := chainHash(raw, body)
		prev = hex.EncodeToString(h[:])
	}
}

func w28_admitted(t *testing.T, r routeResult) {
	t.Helper()
	if r.Decision != decPin || r.Reason != "so-admit" || !r.Covered {
		t.Fatalf("want admitted so-admit pin, got %s", w28_signature(r))
	}
}

// TestSimFA7Singleflight exercises the real bridge/transport with a blocked
// leader POST, a distinct-key negative control and a two-round flight-group
// cleanup proof. It fails if the coalescing is absent, leaks its map entry or
// hands waiters a different answer.
func TestSimFA7Singleflight(t *testing.T) {
	t.Run("same_key_one_post_and_shared_answer", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 1)
		frame := w28_frame(1, "classify singleflight request", "")
		const n = 12
		var ready sync.WaitGroup
		ready.Add(n)
		start := make(chan struct{})
		results := make(chan routeResult, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-start
				results <- d.decideFrame(frame)
			}()
		}
		ready.Wait()
		close(start)
		if got := w28_waitEnter(t, stub); got != 1 {
			t.Fatalf("first POST signalled as %d, want 1", got)
		}
		time.Sleep(30 * time.Millisecond)
		if got := stub.count(); got != 1 {
			t.Fatalf("SO POSTs while leader blocked = %d, want 1", got)
		}
		if got := len(results); got != 0 {
			t.Fatalf("%d waiters answered before leader released", got)
		}
		stub.release()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		w28_waitAll(t, done, "all coalesced callers")
		close(results)
		first := true
		var want string
		seen := 0
		for r := range results {
			sig := w28_signature(r)
			w28_admitted(t, r)
			if r.ConfBucket != confBucket(0.95) {
				t.Fatalf("wrong confidence bucket: %s", sig)
			}
			if first {
				want, first = sig, false
			} else if sig != want {
				t.Fatalf("waiter got a different answer: %s vs %s", sig, want)
			}
			seen++
		}
		if seen != n {
			t.Fatalf("collected %d results, want %d", seen, n)
		}
		if got := stub.count(); got != 1 {
			t.Fatalf("SO POSTs after all callers = %d, want 1", got)
		}
		w28_checkReceiptChain(t)
	})

	t.Run("distinct_keys_two_posts", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 0)
		a := w28_frame(10, "classify alpha request", "")
		b := w28_frame(11, "classify beta request", "")
		fa, errA := decodeFrame(a)
		fb, errB := decodeFrame(b)
		if errA != nil || errB != nil {
			t.Fatalf("decode: %v %v", errA, errB)
		}
		ka := gateKeyBytes(unwrapParams(a, fa), soCapability())
		kb := gateKeyBytes(unwrapParams(b, fb), soCapability())
		if ka == kb {
			t.Fatal("negative control invalid: distinct states produced the same gate key")
		}
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		results := make(chan routeResult, 2)
		var wg sync.WaitGroup
		for _, frame := range [][]byte{a, b} {
			wg.Add(1)
			go func(raw []byte) {
				defer wg.Done()
				ready.Done()
				<-start
				results <- d.decideFrame(raw)
			}(frame)
		}
		ready.Wait()
		close(start)
		w28_waitEnter(t, stub)
		w28_waitEnter(t, stub)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		w28_waitAll(t, done, "both distinct keys")
		close(results)
		for r := range results {
			w28_admitted(t, r)
		}
		if got := stub.count(); got != 2 {
			t.Fatalf("SO POSTs for 2 distinct keys = %d, want 2", got)
		}
	})

	t.Run("group_cleanup_and_shared_value", func(t *testing.T) {
		g := newSOFlightGroup()
		var calls atomic.Int32
		entered := make(chan int, 2)
		releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		fn := func() routeResult {
			i := int(calls.Add(1)) - 1
			entered <- i
			<-releases[i]
			return routeResult{Decision: decPin, Reason: "sim" + strconv.Itoa(i)}
		}
		k := [32]byte{7, 3, 1}
		const n = 8
		for round := 0; round < 2; round++ {
			var ready sync.WaitGroup
			ready.Add(n)
			start := make(chan struct{})
			results := make(chan routeResult, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ready.Done()
					<-start
					results <- g.do(k, fn)
				}()
			}
			ready.Wait()
			close(start)
			select {
			case got := <-entered:
				if got != round {
					t.Fatalf("round %d entered leader %d", round, got)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("round %d: no leader entered fn", round)
			}
			time.Sleep(20 * time.Millisecond)
			close(releases[round])
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			w28_waitAll(t, done, "flight group round "+strconv.Itoa(round))
			close(results)
			want := "sim" + strconv.Itoa(round)
			seen := 0
			for r := range results {
				if r.Decision != decPin || r.Reason != want {
					t.Fatalf("round %d waiter got %s, want %s", round, w28_signature(r), want)
				}
				seen++
			}
			if seen != n {
				t.Fatalf("round %d collected %d results, want %d", round, seen, n)
			}
			if got := int(calls.Load()); got != round+1 {
				t.Fatalf("round %d fn calls = %d, want %d (map entry not cleared)", round, got, round+1)
			}
		}
	})
}

func TestSimFA8StickyPinStrictHit(t *testing.T) {
	t.Run("strict_hit_zero_posts", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 0)
		frame := w28_frame(1, "classify sticky request", "")
		r1 := d.decideFrame(frame)
		w28_admitted(t, r1)
		if stub.count() != 1 {
			t.Fatalf("first decision made %d POSTs, want 1", stub.count())
		}
		genAfterFirst := d.pin.generation()
		r2 := d.decideFrame(frame)
		r3 := d.decideFrame(frame)
		t.Logf("generation after first admit=%d; replay1=%s; replay2=%s; posts=%d",
			genAfterFirst, w28_signature(r2), w28_signature(r3), stub.count())
		if r3.Reason != "cache-hit" || r3.Decision != decPin {
			t.Fatalf("cache eventually broken: %s", w28_signature(r3))
		}
		if r3.ConfBucket != r1.ConfBucket || r3.LastDecision.Conf != 0 {
			t.Fatalf("cache hit lost typed/bucket-only decision: %s", w28_signature(r3))
		}
		if got := stub.count(); got != 1 {
			t.Fatalf("POSTs after three identical decisions = %d, want 1 (strict immediate hit)", got)
		}
		if r2.Decision != decPin || r2.Reason != "cache-hit" || !r2.Covered {
			t.Errorf("FA8 BROKEN: strict hit missed on immediate replay: %s", w28_signature(r2))
		}
		if got := stub.count(); got != 1 {
			t.Errorf("FA8 BROKEN: immediate replay made %d extra SO POST, want 0", got-1)
		}
	})

	t.Run("volatile_fields_still_hit", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 0)
		first := w28_frame(1, "classify sticky request", `,"timestamp":1,"session_id":"s-1","client":"claude"`)
		second := w28_frame(999, "classify sticky request", `,"timestamp":987654321,"session-id":"s-99999","clientName":"cursor"`)
		w28_admitted(t, d.decideFrame(first))
		r := d.decideFrame(second)
		if r.Reason != "cache-hit" || r.Decision != decPin {
			t.Fatalf("volatile-only change broke strict hit: %s", w28_signature(r))
		}
		if got := stub.count(); got != 1 {
			t.Fatalf("SO POSTs = %d, want 1", got)
		}
	})

	t.Run("boundary_0_8_admit_below_abstain", func(t *testing.T) {
		d1, s1 := w28_env(t, w28_answer(0.8), 0)
		r1 := d1.decideFrame(w28_frame(1, "classify boundary request", ""))
		w28_admitted(t, r1)
		if r1.ConfBucket != confBucket(0.8) || r1.ConfBucket != "0.8" {
			t.Fatalf("0.80 bucket = %q, want 0.8", r1.ConfBucket)
		}
		if s1.count() != 1 {
			t.Fatalf("0.80 POSTs = %d, want 1", s1.count())
		}
		d2, s2 := w28_env(t, w28_answer(0.79), 0)
		r2 := d2.decideFrame(w28_frame(1, "classify boundary request", ""))
		if r2.Abstained != true || r2.Decision != decPass || r2.Reason != "so-low-conf" {
			t.Fatalf("0.79 should abstain below cut: %s", w28_signature(r2))
		}
		if s2.count() != 1 {
			t.Fatalf("0.79 POSTs = %d, want 1", s2.count())
		}
	})

	t.Run("hysteresis_hold_after_0_8", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 0)
		w28_admitted(t, d.decideFrame(w28_frame(1, "classify first request", "")))
		stub.setAnswer(w28_answer(0.79))
		r := d.decideFrame(w28_frame(2, "classify second request", ""))
		if r.Decision != decPin || !r.Covered {
			t.Errorf("FA8 BROKEN: no decision-level hysteresis; 0.79 re-vote on a new state drops pin: %s",
				w28_signature(r))
		}
	})

	t.Run("pin_rotation_invalidates_cached_admit", func(t *testing.T) {
		d, stub := w28_env(t, w28_answer(0.95), 0)
		frame := w28_frame(1, "classify rotation request", "")
		w28_admitted(t, d.decideFrame(frame))
		if stub.count() != 1 {
			t.Fatalf("first POSTs = %d, want 1", stub.count())
		}
		g0 := d.pin.generation()
		d.pin.markSuccess(d.pin.current().Name)
		if d.pin.generation() != g0 {
			t.Fatal("same-name markSuccess bumped generation; sticky pin is not sticky")
		}
		if r := d.decideFrame(frame); r.Reason != "cache-hit" {
			t.Fatalf("same generation missed cache: %s", w28_signature(r))
		}
		if stub.count() != 1 {
			t.Fatalf("sticky hit made an extra POST")
		}
		if d.pin.rotateOnFailure(); d.pin.generation() <= g0 {
			t.Fatal("rotateOnFailure did not bump generation")
		}
		w28_admitted(t, d.decideFrame(frame))
		if got := stub.count(); got != 2 {
			t.Fatalf("POSTs after pin rotation = %d, want 2", got)
		}
	})

	t.Run("wire_prefix_stable_across_session_change", func(t *testing.T) {
		_, stub := w28_env(t, w28_answer(0.95), 0)
		d1, d2 := newDecisionBridge(), newDecisionBridge()
		w28_admitted(t, d1.decideFrame(w28_frame(1, "classify wire request", `,"session_id":"s-1","timestamp":1`)))
		w28_admitted(t, d2.decideFrame(w28_frame(2, "classify wire request", `,"session_id":"s-2","timestamp":2`)))
		bodies := stub.all()
		if len(bodies) != 2 {
			t.Fatalf("POSTs = %d, want 2", len(bodies))
		}
		if bodies[0] != bodies[1] {
			t.Fatalf("FA8 BROKEN: wire body changed with volatile session fields\na=%s\nb=%s", bodies[0], bodies[1])
		}
	})

	// Genuine falsifier for FA8's stable identifier requirement. The only
	// outbound decision POST this artifact makes is askSOQOnce; no Go source
	// emits session_id or prompt_cache_key anywhere. This subtest is expected
	// to FAIL until production carries a stable identifier on the wire.
	t.Run("stable_session_id_prompt_cache_key_present", func(t *testing.T) {
		_, stub := w28_env(t, w28_answer(0.95), 0)
		d := newDecisionBridge()
		w28_admitted(t, d.decideFrame(w28_frame(1, "classify wire request", "")))
		bodies := stub.all()
		if len(bodies) != 1 {
			t.Fatalf("POSTs = %d, want 1", len(bodies))
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(bodies[0]), &top); err != nil {
			t.Fatalf("SO POST is not a JSON object: %v", err)
		}
		keys := make([]string, 0, len(top))
		for k := range top {
			keys = append(keys, k)
		}
		_, hasSession := top["session_id"]
		_, hasCacheKey := top["prompt_cache_key"]
		if !hasSession || !hasCacheKey {
			t.Fatalf("FA8 BROKEN: SO POST has no stable session_id/prompt_cache_key "+
				"(session_id=%v prompt_cache_key=%v, top-level keys=%v); "+
				"body=%s", hasSession, hasCacheKey, keys, bodies[0])
		}
		if strings.TrimSpace(bodies[0]) == "" {
			t.Fatal("empty SO POST body")
		}
	})
}
