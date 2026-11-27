package app

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Slice-07: two slow-but-successful samples opened the router breaker through
// the latency branch with no cooldown or probe, so askSOQ failed open and
// RunCombo skipped the tool for the whole process (never a recorded success).

func simS07Env(t *testing.T) {
	simN28Setup(t)
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "3") // slow latency alone trips
	t.Setenv("WALSY_ROUTER_LATENCY_THRESHOLD_MS", "1")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "20ms")
	routerReset()
}

func TestSimS07LatencyBreakerHalfOpens(t *testing.T) {
	simS07Env(t)

	routerRecord("systemone", true, 5*time.Millisecond)
	routerRecord("systemone", true, 5*time.Millisecond)
	if !routerBreakerOpen("systemone") {
		t.Fatal("two slow samples did not open the latency circuit")
	}
	if !routerBreakerOpen("systemone") {
		t.Fatal("open circuit admitted a call before cooldown")
	}

	time.Sleep(30 * time.Millisecond)
	if routerBreakerOpen("systemone") {
		t.Fatalf("latency-opened breaker never half-opens after cooldown: SO stays skipped for the process")
	}
	if !routerBreakerOpen("systemone") {
		t.Fatal("half-open probe admitted a second concurrent call")
	}

	routerRecord("systemone", true, 100*time.Microsecond)
	if routerBreakerOpen("systemone") {
		t.Fatal("successful half-open probe did not close the latency circuit")
	}
}

func TestSimS07SlowSOSuccessDoesNotSilenceGate(t *testing.T) {
	simS07Env(t)
	var hits int32
	simN28Stub(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(5 * time.Millisecond) // slow but valid answer
		_, _ = io.WriteString(w, simN28ValidSO)
	})

	for i := 0; i < 2; i++ {
		if d := askSO("classify this"); d.Choice != "yes" {
			t.Fatalf("slow SO answer %d = %+v", i+1, d)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("slow SO posts = %d, want 2", got)
	}

	time.Sleep(30 * time.Millisecond) // cooldown permits one half-open probe
	if d := askSO("classify this"); d.Choice != "yes" {
		t.Fatalf("latency-opened breaker never probes again: decision = %+v", d)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("posts after cooldown = %d, want 3", got)
	}
	if routerBreakerOpen("systemone") {
		t.Fatal("successful probe left the latency circuit open")
	}
}
