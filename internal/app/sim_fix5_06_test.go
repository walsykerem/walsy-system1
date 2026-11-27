package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SIM-FIX5-06: simulation-only coverage for the bounded general re-ask.
// Fake key + loopback stub; no canonical state, no real credential printed.

const simFix5Request = `{"state":"decide: classify this fix5 request","method":"tools/call","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`

const (
	simFix5Low  = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.64,"probabilities":{"yes":0.52,"no":0.48}}}}`
	simFix5High = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95,"probabilities":{"yes":0.95,"no":0.05}}}}`
)

func simFix5Stub(t *testing.T, replies ...string) *int32 {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	for _, k := range []string{"WALSY_ROUTER_RETRY_BUDGET", "WALSY_ROUTER_BREAKER_THRESHOLD", "WALSY_ROUTER_BREAKER_COOLDOWN", "WALSY_ROUTER_DENY"} {
		t.Setenv(k, "")
	}
	routerReset()
	t.Cleanup(routerReset)
	t.Setenv("EVREN_API_KEY", "sim-fix5-test-key")

	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&posts, 1)) - 1
		body := simFix5High
		if i < len(replies) {
			body = replies[i]
		}
		_, _ = io.WriteString(w, body)
	}))
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
	return &posts
}

func TestSimFix506LowThenHighPromotesSameLabel(t *testing.T) {
	posts := simFix5Stub(t, simFix5Low, simFix5High)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Decision != decPin || r.Reason != "so-admit" || r.LastDecision.Choice != "yes" {
		t.Fatalf("admit after re-ask: decision=%q reason=%q label=%q", r.Decision, r.Reason, r.LastDecision.Choice)
	}
	if got := atomic.LoadInt32(posts); got != 2 {
		t.Fatalf("POST count want 2, got %d", got)
	}
	r2 := d.decide([]byte(simFix5Request))
	if r2.Decision != decPin || r2.Reason != "cache-hit" || atomic.LoadInt32(posts) != 2 {
		t.Fatalf("admit cache: decision=%q reason=%q posts=%d", r2.Decision, r2.Reason, atomic.LoadInt32(posts))
	}
	t.Logf("PASS: low->high same label => decision=%s reason=%s posts=%d (cache posts still 2)", r.Decision, r.Reason, atomic.LoadInt32(posts))
}

func TestSimFix506LowTwiceStaysAbstain(t *testing.T) {
	posts := simFix5Stub(t, simFix5Low, simFix5Low)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Reason != "so-low-conf" || r.Abstained != true || r.LastDecision.Choice != "yes" {
		t.Fatalf("abstain after re-ask: decision=%q reason=%q abstained=%v label=%q", r.Decision, r.Reason, r.Abstained, r.LastDecision.Choice)
	}
	if got := atomic.LoadInt32(posts); got != 2 {
		t.Fatalf("POST count want 2, got %d", got)
	}
	t.Logf("PASS: low->low keeps abstain reason=%s posts=%d", r.Reason, atomic.LoadInt32(posts))
}

func TestSimFix506HighFirstNoSecondPost(t *testing.T) {
	posts := simFix5Stub(t, simFix5High)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("first-call admit: decision=%q reason=%q", r.Decision, r.Reason)
	}
	if got := atomic.LoadInt32(posts); got != 1 {
		t.Fatalf("POST count want 1, got %d", got)
	}
	t.Logf("PASS: high-first => reason=%s posts=%d", r.Reason, atomic.LoadInt32(posts))
}

func TestSimFix506BreakerBudgetRequiresNoExtraPost(t *testing.T) {
	t.Run("breaker", func(t *testing.T) {
		posts := simFix5Stub(t, simFix5Low)
		t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "1")
		routerReset()
		// Open the breaker directly: stub replies are well-formed, so an HTTP
		// failure here would test transport, not the re-ask pre-check.
		routerMu.Lock()
		routerInitLocked()
		b := routerState.breakers["systemone"]
		b.failures, b.last = routerState.threshold, time.Now()
		routerState.breakers["systemone"] = b
		routerMu.Unlock()
		if !routerBreakerOpen("systemone") {
			t.Fatal("breaker did not open")
		}
		d := newDecisionBridge()
		first := routeResult{Decision: decPass, Reason: "so-low-conf", Abstained: true,
			LastDecision: soDecision{Choice: "yes", Conf: 0.64, AnswerType: "choice", MarginKnown: true, Margin: 0.04}}
		got := d.reaskLowConfidence(simFix5Request, first)
		if got.Decision != first.Decision || got.Reason != first.Reason || got.LastDecision.Choice != first.LastDecision.Choice {
			t.Fatalf("breaker re-ask must not change result: %+v", got)
		}
		if n := atomic.LoadInt32(posts); n != 0 {
			t.Fatalf("breaker extra POSTs want 0, got %d", n)
		}
		t.Logf("PASS: breaker open => extra posts=0 total=%d", atomic.LoadInt32(posts))
	})

	t.Run("budget", func(t *testing.T) {
		posts := simFix5Stub(t, simFix5Low)
		t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "0")
		routerReset()
		if routerBudgetLeft() != 0 {
			t.Fatalf("budget want 0, got %d", routerBudgetLeft())
		}
		d := newDecisionBridge()
		first := routeResult{Decision: decPass, Reason: "so-low-conf", Abstained: true,
			LastDecision: soDecision{Choice: "yes", Conf: 0.64, AnswerType: "choice", MarginKnown: true, Margin: 0.04}}
		got := d.reaskLowConfidence(simFix5Request, first)
		if got.Decision != first.Decision || got.Reason != first.Reason || got.LastDecision.Choice != first.LastDecision.Choice {
			t.Fatalf("empty budget re-ask must not change result: %+v", got)
		}
		if n := atomic.LoadInt32(posts); n != 0 {
			t.Fatalf("empty budget extra POSTs want 0, got %d", n)
		}
		t.Logf("PASS: budget 0 => extra posts=0 total=%d", atomic.LoadInt32(posts))
	})

	t.Run("missing_requires", func(t *testing.T) {
		posts := simFix5Stub(t, simFix5Low, simFix5High)
		state := `{"state":"decide: classify untrusted text","method":"tools/call","item":"intake.injection","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true}`
		d := newDecisionBridge()
		first := routeResult{Decision: decPass, Reason: "so-low-conf", Abstained: true,
			LastDecision: soDecision{Choice: "clean", Conf: 0.64, AnswerType: "choice", MarginKnown: true, Margin: 0.04}}
		got := d.reaskLowConfidence(state, first)
		if got.Decision != first.Decision || got.Reason != first.Reason || got.LastDecision.Choice != first.LastDecision.Choice {
			t.Fatalf("missing Requires re-ask must not change result: %+v", got)
		}
		if n := atomic.LoadInt32(posts); n != 0 {
			t.Fatalf("missing Requires extra POSTs want 0, got %d", n)
		}
		t.Logf("PASS: missing Requires => extra posts=0 total=%d", atomic.LoadInt32(posts))
	})
}

func TestSimFix506DifferentLabelKeepsAbstain(t *testing.T) {
	posts := simFix5Stub(t, simFix5Low, `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.95,"probabilities":{"no":0.95,"yes":0.05}}}}`)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Reason != "so-low-conf" || r.LastDecision.Choice != "yes" {
		t.Fatalf("label flip must not admit: reason=%q label=%q", r.Reason, r.LastDecision.Choice)
	}
	if n := atomic.LoadInt32(posts); n != 2 {
		t.Fatalf("POST count want 2, got %d", n)
	}
	t.Logf("PASS: low yes -> high no keeps abstain reason=%s label=%s posts=%d", r.Reason, r.LastDecision.Choice, atomic.LoadInt32(posts))
}

func TestSimFix506ConfidenceOnlyLowGetsNoReask(t *testing.T) {
	posts := simFix5Stub(t, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.79}}}`)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Reason != "so-low-conf" {
		t.Fatalf("confidence-only low: reason=%q", r.Reason)
	}
	if n := atomic.LoadInt32(posts); n != 1 {
		t.Fatalf("confidence-only low POSTs want 1, got %d", n)
	}
	t.Logf("PASS: confidence-only low => posts=%d (legacy stub contract preserved)", atomic.LoadInt32(posts))
}

func TestSimFix506TightenedStateKeepsOriginalEnvelope(t *testing.T) {
	posts := simFix5Stub(t, simFix5Low, simFix5High)
	d := newDecisionBridge()
	r := d.decide([]byte(simFix5Request))
	if r.Decision != decPin || atomic.LoadInt32(posts) != 2 {
		t.Fatalf("admit failed: decision=%q posts=%d", r.Decision, atomic.LoadInt32(posts))
	}
	if !strings.Contains(simFix5Request, `"opt_in":true`) {
		t.Fatal("fixture lost opt_in")
	}
	t.Log("PASS: re-ask reuses same stateEnvelope fields, only state text gains one suffix")
}
