package app

import (
	"testing"
	"time"
)

// Slice-05: provenance.go:116 calls routerBreakerOpen("systemone") just to
// report "breaker_open". That read consumes the single half-open probe without
// recording a result, so slice-07's `if b.halfOpen { return true }` wedges the
// breaker forever and the SO gate (gate.go:365) never runs again. The probe
// must be a lease: it expires after one cooldown.
func TestSimS05ProbeLeaseExpires(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "200ms")
	routerReset()

	routerRecord("systemone", false)
	routerRecord("systemone", false)
	time.Sleep(250 * time.Millisecond)

	if routerBreakerOpen("systemone") {
		t.Fatal("cooldown must grant a probe to the status read")
	}
	if !routerBreakerOpen("systemone") {
		t.Fatal("second caller must wait while the probe is outstanding")
	}
	time.Sleep(250 * time.Millisecond)
	if routerBreakerOpen("systemone") {
		t.Fatal("probe consumed by a status read wedged the breaker forever")
	}
}
