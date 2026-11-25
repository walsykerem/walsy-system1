package main

import (
	"bytes"
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

// Simulation only. No production file is touched and no external SO endpoint is
// contacted: the shipped gate is pointed at a loopback httptest server. The key
// is a local dummy so soAPIKey never reads the machine's registry or secrets.

const simT1T2Admit = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`
const simT1T2HTTP500 = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`

type simT1T2SO struct {
	posts     atomic.Int64
	inFlight  atomic.Int64
	maxFlight atomic.Int64
	hold      chan struct{}
	mu        sync.Mutex
	bodies    []map[string]any
	status    func(n int) int
	reply     func(n int) string
}

func (s *simT1T2SO) serve(w http.ResponseWriter, r *http.Request) {
	n := int(s.posts.Add(1))
	cur := s.inFlight.Add(1)
	for {
		m := s.maxFlight.Load()
		if cur <= m || s.maxFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	if s.hold != nil {
		<-s.hold
	}
	code := http.StatusOK
	text := simT1T2Admit
	if s.status != nil {
		code = s.status(n)
	}
	if s.reply != nil {
		text = s.reply(n)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, text)
}

func (s *simT1T2SO) count() int { return int(s.posts.Load()) }

func (s *simT1T2SO) lastBody() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

func simT1T2Boot(t *testing.T, stub *simT1T2SO) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "simulation-only-key")
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	oldURL, oldTO := soEndpoint, soTimeout
	soEndpoint, soTimeout = srv.URL, 1500*time.Millisecond
	routerReset()
	t.Cleanup(func() {
		soEndpoint, soTimeout = oldURL, oldTO
		srv.Close()
		routerReset()
	})
	return srv
}

func simT1T2State(text, item string) string {
	b, _ := json.Marshal(stateEnvelope{
		State: text, Method: "tools/call", NeedLabel: true, NoScratch: true,
		Safe: true, Reversible: true, OptIn: true, Phase: "before-model",
		Authority: true, Calibrated: true, Item: item,
	})
	return string(b)
}

func simT1T2Caps(phase string) map[string]any {
	return map[string]any{
		"need_label": true, "no_scratch": true, "safe": true, "reversible": true,
		"authority": true, "calibrated": true, "opt_in": true, "phase": phase,
	}
}

func simT1T2Frame(method string, params map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return b
}

func simT1T2Wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// T1, side A: one bounded POST per decision, no retry. Three degraded calls
// followed by one good call must produce exactly four POSTs, never a burst
// inside one call. The breaker closes again on the success.
func TestSimT1SingleBoundedPost(t *testing.T) {
	// Keep the breaker shut so the fourth call really reaches SO: this test
	// isolates bounded POST behavior, while breaker behavior has its own test.
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "100")
	stub := &simT1T2SO{status: func(n int) int {
		if n < 4 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}}
	simT1T2Boot(t, stub)
	for i := 1; i <= 3; i++ {
		r := route(simT1T2State("classify request "+string(rune('a'+i)), ""), nil)
		if r.Decision != decPass || !r.Abstained || r.Reason != "so-degraded" {
			t.Fatalf("degraded call %d: %+v", i, r)
		}
		if got := stub.count(); got != i {
			t.Fatalf("call %d produced %d POSTs, want %d (retry/burst)", i, got, i)
		}
	}
	r := route(simT1T2State("classify request d", ""), nil)
	if r.Decision != decPin || !r.Covered {
		t.Fatalf("recovered call: %+v", r)
	}
	if got := stub.count(); got != 4 {
		t.Fatalf("total POSTs = %d, want 4", got)
	}
	if routerBreakerOpen("systemone") {
		t.Fatal("breaker stayed open after a success")
	}
}

// T1, side B: the SO gate is coupled to the shared router breaker. After two
// failures at threshold 2 the third request must fail open with no POST at all;
// after a simulated cooldown one half-open probe is allowed.
func TestSimT1BreakerGatesSO(t *testing.T) {
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
	stub := &simT1T2SO{status: func(int) int { return http.StatusInternalServerError }}
	simT1T2Boot(t, stub)

	for i := 0; i < 2; i++ {
		r := route(simT1T2State("classify breaker case "+string(rune('a'+i)), ""), nil)
		if r.Reason != "so-degraded" {
			t.Fatalf("failure %d: %+v", i, r)
		}
	}
	if stub.count() != 2 || !routerBreakerOpen("systemone") {
		t.Fatalf("breaker did not open: posts=%d open=%v", stub.count(), routerBreakerOpen("systemone"))
	}
	blocked := route(simT1T2State("classify breaker case c", ""), nil)
	if blocked.Reason != "so-degraded" || !blocked.Abstained {
		t.Fatalf("open breaker result: %+v", blocked)
	}
	if stub.count() != 2 {
		t.Fatalf("open breaker still posted: %d", stub.count())
	}

	routerMu.Lock()
	b := routerState.breakers["systemone"]
	b.last = time.Now().Add(-2 * time.Hour)
	b.halfOpen = false
	routerState.breakers["systemone"] = b
	routerMu.Unlock()

	probe := route(simT1T2State("classify breaker case d", ""), nil)
	if probe.Reason != "so-degraded" || stub.count() != 3 {
		t.Fatalf("half-open probe: %+v posts=%d", probe, stub.count())
	}
	// A failed probe resets the failure window: the next call is allowed again.
	next := route(simT1T2State("classify breaker case e", ""), nil)
	if next.Reason != "so-degraded" || stub.count() != 4 {
		t.Fatalf("post-cooldown call: %+v posts=%d", next, stub.count())
	}
}

// T1, side C: SO also reads the shared retry budget. Exhausting it elsewhere
// suppresses every SO POST. The log line records the accounting gap: SO never
// spends a token of its own, so #22 (retry coupling) is still only partial.
func TestSimT1BudgetGatesSO(t *testing.T) {
	stub := &simT1T2SO{}
	simT1T2Boot(t, stub)
	for i := 0; i < routerDefaultBudget; i++ {
		if _, _, spent := routerBudgetSpend(); !spent {
			t.Fatalf("budget spent early at %d", i)
		}
	}
	if left := routerBudgetLeft(); left != 0 {
		t.Fatalf("budget left = %d, want 0", left)
	}
	blocked := route(simT1T2State("classify budget case a", ""), nil)
	if blocked.Reason != "so-degraded" || stub.count() != 0 {
		t.Fatalf("empty budget result: %+v posts=%d", blocked, stub.count())
	}
	routerBudgetReset()
	admitted := route(simT1T2State("classify budget case b", ""), nil)
	if admitted.Decision != decPin || stub.count() != 1 {
		t.Fatalf("refilled budget result: %+v posts=%d", admitted, stub.count())
	}
	t.Logf("SO call left retry budget at %d of %d (no SO token spent)", routerBudgetLeft(), routerState.budgetMax)
}

// T1, side D: two live SO calls may be in flight; a third must fail open before
// any POST because soSem is full. The stub blocks so the overlap is observed,
// not inferred.
func TestSimT1ConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	stub := &simT1T2SO{hold: release}
	simT1T2Boot(t, stub)

	type outcome struct{ r routeResult }
	out := make(chan outcome, 2)
	for _, name := range []string{"a", "b"} {
		go func(n string) {
			out <- outcome{route(simT1T2State("classify concurrent case "+n, ""), nil)}
		}(name)
	}
	simT1T2Wait(t, "two in-flight SO calls", func() bool { return stub.inFlight.Load() == 2 })
	third := route(simT1T2State("classify concurrent case c", ""), nil)
	if third.Reason != "so-degraded" || stub.count() != 2 {
		t.Fatalf("third call bypassed cap: %+v posts=%d", third, stub.count())
	}
	unblock()
	for i := 0; i < 2; i++ {
		got := <-out
		if got.r.Decision != decPin {
			t.Fatalf("capped call %d: %+v", i, got.r)
		}
	}
	if got := stub.maxFlight.Load(); got != 2 {
		t.Fatalf("max in-flight = %d, want 2", got)
	}
}

// T2: the honest scope. The real decision hook must call SO only for in-scope
// JSON-RPC methods with a model-before phase and complete opt-in. Every other
// frame is a local, fail-open pass with a named reason and zero POSTs.
func TestSimT2ScopeMatrix(t *testing.T) {
	stub := &simT1T2SO{}
	simT1T2Boot(t, stub)

	type tc struct {
		name   string
		method string
		params map[string]any
		reason string
		posts  int
	}
	withState := func(phase, state string) map[string]any {
		p := simT1T2Caps(phase)
		p["state"] = state
		return p
	}
	missingCap := withState("before-model", "classify the missing cap")
	delete(missingCap, "opt_in")
	cases := []tc{
		{"other method", "tools/list", withState("before-model", "classify the list"), "layer0-scope", 0},
		{"post-model phase", "tools/call", withState("after-model", "classify the result"), "layer0-scope", 0},
		{"no opt-in", "tools/call", missingCap, "layer0-opt-in", 0},
		{"static word", "tools/call", withState("before-model", "status"), "layer0-static", 0},
		{"before_model spelling", "initialize", withState("before_model", "classify the request"), "so-admit", 1},
	}
	for _, c := range cases {
		before := stub.count()
		r := newDecisionBridge().decideFrame(simT1T2Frame(c.method, c.params))
		if r.Reason != c.reason {
			t.Errorf("%s: reason=%q want %q (%+v)", c.name, r.Reason, c.reason, r)
		}
		if got := stub.count() - before; got != c.posts {
			t.Errorf("%s: %d SO POSTs, want %d", c.name, got, c.posts)
		}
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("scope matrix total POSTs = %d, want 1", got)
	}
}

// T2, transport proof: advisory SO runs after the response is flushed. A
// blocked SO stub must not delay or 503 the client, and an out-of-scope frame
// must not reach SO at all.
func TestSimT2HTTPPostFlush(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	stub := &simT1T2SO{hold: release}
	simT1T2Boot(t, stub)
	srv := httptest.NewServer(mcpHTTPHandler())
	t.Cleanup(srv.Close)
	// Five seconds so a slow advisory SO call is measured, not mistaken for a
	// client timeout: the client deadline must not mask the flush behavior.
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(frame []byte) (int, string) {
		t.Helper()
		resp, err := client.Post(srv.URL, "application/json", bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("POST frame: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read POST response: %v", err)
		}
		return resp.StatusCode, string(b)
	}

	outFrame := simT1T2Frame("tools/list", simT1T2Caps("before-model"))
	code, body := post(outFrame)
	if code != http.StatusOK || !strings.Contains(body, "tools/list") {
		t.Fatalf("out-of-scope frame: %d %s", code, body)
	}
	if stub.count() != 0 {
		t.Fatalf("out-of-scope frame reached SO %d times", stub.count())
	}

	inParams := simT1T2Caps("before-model")
	inParams["state"] = "classify the http frame"
	inFrame := simT1T2Frame("tools/call", inParams)
	start := time.Now()
	code, body = post(inFrame)
	elapsed := time.Since(start)
	if code != http.StatusOK || !strings.Contains(body, "classify the http frame") {
		t.Fatalf("in-scope frame was not echoed: %d %s", code, body)
	}
	// Gap catcher: mcpHTTPHandlerUpstream writes+flushes the body but only
	// returns after advisoryHTTP, so a chunked response body is not terminated
	// until SO finishes. Fix in production: set Content-Length before
	// WriteHeader, or run advisoryHTTP in a goroutine after Flush.
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("response body waited %s for blocked SO (flush did not complete the body)", elapsed)
	}
	t.Logf("in-scope response returned in %s while SO was held", elapsed)
	simT1T2Wait(t, "one SO POST after flush", func() bool { return stub.count() == 1 })
	if stub.inFlight.Load() != 1 {
		t.Fatalf("SO in-flight = %d, want 1 (stub should still be held)", stub.inFlight.Load())
	}
	unblock()
	simT1T2Wait(t, "SO call to finish", func() bool { return stub.inFlight.Load() == 0 })
}

// T2, the 58 fixed items: a gated item sends its own label set on the wire, a
// non-gate item is answered locally with no SO call.
func TestSimT2RegistryItems(t *testing.T) {
	items := loadDecisionItems()
	if len(items) != 58 {
		t.Fatalf("registry has %d items, want 58", len(items))
	}
	stub := &simT1T2SO{reply: func(int) string {
		return `{"answers":{"gate":{"type":"choice","choice":"sufficient","confidence":0.9}}}`
	}}
	simT1T2Boot(t, stub)

	r := route(simT1T2State("decide whether the context is sufficient", "context.sufficient"), nil)
	if r.Decision != decPin || r.Label != "sufficient" || stub.count() != 1 {
		t.Fatalf("gated item: %+v posts=%d", r, stub.count())
	}
	body := stub.lastBody()
	questions, ok := body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("wire questions = %T", body["questions"])
	}
	q, ok := questions["gate"].(map[string]any)
	if !ok {
		t.Fatalf("wire gate question = %T", questions["gate"])
	}
	criteria, ok := q["criteria"].(map[string]any)
	if !ok {
		t.Fatalf("wire criteria = %T", q["criteria"])
	}
	if _, ok := criteria["sufficient"]; !ok {
		t.Fatalf("item labels missing on wire: %v", criteria)
	}
	if _, ok := criteria["yes"]; ok {
		t.Fatalf("generic label leaked into item question: %v", criteria)
	}

	before := stub.count()
	local := route(simT1T2State("classify the local item", "tools.pick"), nil)
	if local.Reason != "registry-local" || stub.count() != before {
		t.Fatalf("non-gate item: %+v posts=%d", local, stub.count())
	}
}
