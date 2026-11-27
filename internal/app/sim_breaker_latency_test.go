package app

import (
	"testing"
	"time"
)

// FA10 follow-up: the latency breaker must honour the same cooldown and single
// half-open probe as the failure breaker. A blocked call records no latency
// sample, so without a probe a slow median would block SO for the whole process.
func TestSimBreakerLatencyRecoversAfterCooldown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_ROUTER_LATENCY_THRESHOLD_MS", "1")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
	routerReset()
	t.Cleanup(routerReset)

	routerRecord("systemone", true, 5*time.Millisecond)
	routerRecord("systemone", true, 5*time.Millisecond)
	if !routerBreakerOpen("systemone") {
		t.Fatal("slow median did not open the breaker")
	}

	routerMu.Lock()
	b := routerState.breakers["systemone"]
	b.last = time.Now().Add(-2 * time.Hour)
	routerState.breakers["systemone"] = b
	routerMu.Unlock()

	if routerBreakerOpen("systemone") {
		t.Fatal("latency breaker never recovered: no half-open probe after cooldown")
	}
	if !routerBreakerOpen("systemone") {
		t.Fatal("half-open probe: second call was not held for the probe result")
	}
}
