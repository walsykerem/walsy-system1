package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fix4-15: the historical incident is commit 22daf0d ("lease the breaker's
// half-open probe"): a latency-opened breaker had no cooldown, so "the tool
// stayed skipped until restart (for "systemone" that kills every SO decision)".
// Loopback-only simulations keep that wedge observable without a real MCP/SO.

const (
	fix4_15SkillOK = "fix4-15 skill ok"
	// shortcut: transport_http.go:304-315 waits up to 100ms after flush for
	// advisory SO; 300ms leaves CI slack. Lower if that window is removed.
	fix4_15MaxAdvisoryOverhead = 300 * time.Millisecond
)

func fix4_15Base(t *testing.T) {
	t.Helper()
	resetRouterForTest(t)
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("WALSY_SO_APPROVER", "sim-fix4-15")
	t.Setenv("EVREN_API_KEY", "sim-fix4-15-key")
	t.Setenv("WALSY_ROUTER_DENY", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "3")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "3")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "50ms")
	routerReset()
}

func fix4_15Frame(id int, needLabel bool, state string) []byte {
	s, _ := json.Marshal(state)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"system1_status","arguments":{},"state":%s,"need_label":%t,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}`, id, s, needLabel))
}

func fix4_15SOAnswer(choice string, conf float64) string {
	b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
		"type": "choice", "choice": choice, "confidence": conf,
	}}})
	return string(b)
}

func fix4_15AttachSO(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = oldEndpoint, oldClient })
}

func fix4_15FastSO(t *testing.T, choice string, posts *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fix4_15SOAnswer(choice, 0.95))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fix4_15SkillUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if !bytes.Contains(b, []byte("system1_status")) {
			t.Errorf("upstream got unexpected frame: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fix4-15 skill ok"}]}}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fix4_15Post(client *http.Client, url string, frame []byte) (time.Duration, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return time.Since(start), body, err
}

func fix4_15AssertSkill(t *testing.T, d time.Duration, body []byte, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("skill call failed after %v: %v", d, err)
	}
	if !strings.Contains(string(body), fix4_15SkillOK) {
		t.Fatalf("skill result dropped/rewritten: %q", body)
	}
}

// A stalled SO decision must not hold a running MCP skill: the frame is
// forwarded before the advisory call and the handler's bounded wait expires.
func TestFix4_15StalledRouterDecisionDoesNotBlockSkill(t *testing.T) {
	fix4_15Base(t)
	var posts atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	so := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		select {
		case <-release:
		case <-r.Context().Done(): // advisory SO has its own timeout
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fix4_15SOAnswer("yes", 0.95))
	}))
	t.Cleanup(so.Close)
	fix4_15AttachSO(t, so)
	router := httptest.NewServer(mcpHTTPHandlerUpstream(fix4_15SkillUpstream(t).URL))
	t.Cleanup(router.Close)
	// LIFO cleanup: release the stalled SO handler before httptest.Close.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := &http.Client{Timeout: 2 * time.Second}

	baseDur, body, err := fix4_15Post(client, router.URL, fix4_15Frame(1, false, "fix4-15 no-label baseline"))
	fix4_15AssertSkill(t, baseDur, body, err)
	if posts.Load() != 0 {
		t.Fatalf("baseline consulted SO %d times", posts.Load())
	}

	slowDur, body, err := fix4_15Post(client, router.URL, fix4_15Frame(2, true, "fix4-15 stalled-oracle probe"))
	fix4_15AssertSkill(t, slowDur, body, err)
	if posts.Load() == 0 {
		t.Fatal("slow case never exercised the router decision path")
	}
	if posts.Load() > 2 {
		t.Fatalf("SO concurrency cap breached: %d posts", posts.Load())
	}
	t.Logf("skill latency: baseline=%v stalled-router=%v overhead=%v posts=%d", baseDur, slowDur, slowDur-baseDur, posts.Load())
	if slowDur > baseDur+fix4_15MaxAdvisoryOverhead {
		t.Fatalf("router stalled skill: slow=%v baseline=%v overhead=%v", slowDur, baseDur, slowDur-baseDur)
	}
}

// Parallel skills stay alive when two SO decisions occupy the semaphore: the
// excess decisions fail open instead of starving the relay.
func TestFix4_15ParallelSkillsSurviveRouterContention(t *testing.T) {
	fix4_15Base(t)
	var posts atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	so := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fix4_15SOAnswer("yes", 0.95))
	}))
	t.Cleanup(so.Close)
	fix4_15AttachSO(t, so)
	router := httptest.NewServer(mcpHTTPHandlerUpstream(fix4_15SkillUpstream(t).URL))
	t.Cleanup(router.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	durs := make([]time.Duration, n)
	bodies := make([][]byte, n)
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			durs[i], bodies[i], errs[i] = fix4_15Post(client, router.URL,
				fix4_15Frame(i+1, true, fmt.Sprintf("fix4-15 parallel skill %d", i)))
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		fix4_15AssertSkill(t, durs[i], bodies[i], errs[i])
		if durs[i] > 2*time.Second {
			t.Fatalf("skill %d starved for %v", i, durs[i])
		}
	}
	if posts.Load() == 0 || posts.Load() > 2 {
		t.Fatalf("SO posts=%d, want 1-2 under contention", posts.Load())
	}
}

// A poisoned breaker and a cached refusal are router state, not a skill veto:
// advisory relay must still deliver the upstream result.
func TestFix4_15PoisonedBreakerAndCachedRefusalDoNotCutSkill(t *testing.T) {
	t.Run("breaker", func(t *testing.T) {
		fix4_15Base(t)
		t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "5s")
		routerReset()
		var posts atomic.Int64
		fix4_15AttachSO(t, fix4_15FastSO(t, "yes", &posts))
		for i := 0; i < 3; i++ {
			routerRecord("systemone", false)
		}
		routerRecord("system1_status", false)
		if !routerBreakerOpen("systemone") {
			t.Fatal("breaker did not open")
		}
		if !routerAllow("system1_status") {
			t.Fatal("skill error poisoned the tool allow decision")
		}
		router := httptest.NewServer(mcpHTTPHandlerUpstream(fix4_15SkillUpstream(t).URL))
		t.Cleanup(router.Close)
		d, body, err := fix4_15Post(&http.Client{Timeout: 2 * time.Second}, router.URL,
			fix4_15Frame(1, true, "fix4-15 breaker probe"))
		fix4_15AssertSkill(t, d, body, err)
		if posts.Load() != 0 {
			t.Fatalf("open breaker still spent %d SO posts", posts.Load())
		}
	})

	t.Run("cache", func(t *testing.T) {
		fix4_15Base(t)
		var posts atomic.Int64
		fix4_15AttachSO(t, fix4_15FastSO(t, "no", &posts))
		router := httptest.NewServer(mcpHTTPHandlerUpstream(fix4_15SkillUpstream(t).URL))
		t.Cleanup(router.Close)
		client := &http.Client{Timeout: 2 * time.Second}
		for i := 0; i < 2; i++ {
			d, body, err := fix4_15Post(client, router.URL, fix4_15Frame(1, true, "fix4-15 cached refusal"))
			fix4_15AssertSkill(t, d, body, err)
		}
		if posts.Load() != 1 {
			t.Fatalf("cached refusal SO posts=%d, want 1", posts.Load())
		}
	})
}

// MCP reconnect is the documented budget-refill boundary, bounded so a loop
// of initialize frames cannot turn the throttle into an unlimited skill run.
func BenchmarkFix4_15RouterLockCost(b *testing.B) {
	routerReset()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = routerAllow("system1_status")
		}
	})
}

func TestFix4_15ReconnectRefillsRouterBudgetBounded(t *testing.T) {
	fix4_15Base(t)
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	var posts atomic.Int64
	fix4_15AttachSO(t, fix4_15FastSO(t, "yes", &posts))
	d := newDecisionBridge()
	call := func(id int) bool {
		_, blocked := d.enforcingGate(fix4_15Frame(id, true, fmt.Sprintf("fix4-15 budget call %d", id)))
		return !blocked
	}
	for i := 1; i <= 3; i++ {
		if !call(i) {
			t.Fatalf("enforcing blocked skill call %d before budget exhaustion", i)
		}
	}
	if call(4) {
		t.Fatal("spent budget did not block the fourth tool call")
	}
	if _, blocked := d.enforcingGate(gateFrame("initialize", 10)); blocked {
		t.Fatal("MCP initialize did not refill the budget")
	}
	if !call(11) {
		t.Fatal("skill call after reconnect was blocked")
	}

	routerReset()
	for i := 0; i < routerMaxRefills; i++ {
		if _, blocked := d.enforcingGate(gateFrame("initialize", 100+i)); blocked {
			t.Fatalf("initialize %d blocked", i)
		}
	}
	for i := 0; i < 3; i++ {
		if !call(200 + i) {
			t.Fatalf("post-refill skill call %d blocked", i)
		}
	}
	if call(203) {
		t.Fatal("unbounded reconnect refilled a spent budget")
	}
}
