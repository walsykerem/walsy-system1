package app

// Simulation-only coverage for MANIFEST FA9 and FA10. Every case drives real
// production code (askSO*, route, decisionBridge, bridgeStdio and the shipped
// walsy-system1 binary) against a loopback httptest SO stub. No live SO call,
// no key file read, no production edit.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const w29_validSO = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`

var w29_question = soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}

func w29_base(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-fa-test-key")
	for _, k := range []string{
		"WALSY_ROUTER_RETRY_BUDGET", "WALSY_ROUTER_BREAKER_THRESHOLD",
		"WALSY_ROUTER_BREAKER_COOLDOWN", "WALSY_ROUTER_DENY", "WALSY_SO_ENDPOINT",
	} {
		t.Setenv(k, "")
	}
	routerReset()
	t.Cleanup(routerReset)
}

func w29_stub(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, &http.Client{} // shipped client, not srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
	return srv
}

// w29_env strips proxy-local config from the inherited environment so a
// spawned binary starts with the same clean state as the in-process cases.
func w29_env(t *testing.T, extra ...string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "HOME="),
			strings.HasPrefix(kv, "WALSY_"),
			strings.HasPrefix(kv, "EVREN_API_KEY="):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+t.TempDir(), "WALSY_SO_MODE=advisory")
	return append(env, extra...)
}

func w29_binary(t *testing.T, bin string, env []string, frames string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-transport", "stdio", "/bin/cat")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(frames)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func w29_frame(id int, state string) string {
	return `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"tools/call","params":{"name":"read_file","state":"` + state + `",` + explicitCaps + `}}` + "\n"
}

func TestW29_FA9DeadlineAndEndpoint(t *testing.T) {
	t.Run("shipped_deadline_is_150ms", func(t *testing.T) {
		w29_base(t)
		// No case in this file has overridden soTimeout at this point.
		if soTimeout < 100*time.Millisecond || soTimeout > 250*time.Millisecond {
			t.Errorf("FA9 BROKEN: shipped soTimeout=%v, want ~150ms (100-250ms)", soTimeout)
		}
	})

	t.Run("context_timeout_returns_promptly", func(t *testing.T) {
		w29_base(t)
		var posted int32
		w29_stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&posted, 1)
			time.Sleep(300 * time.Millisecond)
		})
		oldTimeout := soTimeout
		soTimeout = 40 * time.Millisecond
		t.Cleanup(func() { soTimeout = oldTimeout })

		start := time.Now()
		d := askSOQ("sim state", w29_question)
		elapsed := time.Since(start)
		if !d.TimedOut || d.Choice != "" {
			t.Errorf("FA9 BROKEN: timed-out decision=%+v, want TimedOut and empty label", d)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("FA9 BROKEN: deadline took %v, want <500ms", elapsed)
		}
		if atomic.LoadInt32(&posted) != 1 {
			t.Errorf("FA9: SO stub saw %d POSTs, want 1", atomic.LoadInt32(&posted))
		}
		// Server-side r.Context() is not observable when the handler never
		// reads the request body; prompt client return is the no-leak proof.
	})

	t.Run("keepalive_reuses_connection", func(t *testing.T) {
		w29_base(t)
		var newConns int32
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, w29_validSO)
		}))
		srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				atomic.AddInt32(&newConns, 1)
			}
		}
		srv.Start()
		oldEndpoint, oldClient := soEndpoint, soClient
		soEndpoint, soClient = srv.URL, &http.Client{}
		t.Cleanup(func() {
			soEndpoint, soClient = oldEndpoint, oldClient
			srv.Close()
		})
		for i := 0; i < 2; i++ {
			if d := askSOQOnce("sim state", w29_question); d.Choice != "yes" {
				t.Fatalf("SO call %d=%+v, want yes", i, d)
			}
		}
		if got := atomic.LoadInt32(&newConns); got != 1 {
			t.Errorf("FA9 keep-alive: TCP connections for 2 SO calls=%d, want 1", got)
		}
	})

	t.Run("cheapest_endpoint_first", func(t *testing.T) {
		w29_base(t)
		var slowHits, fastHits int32
		fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&fastHits, 1)
			_, _ = io.WriteString(w, w29_validSO)
		}))
		defer fast.Close()
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&slowHits, 1)
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, w29_validSO)
		}))
		defer slow.Close()

		// Operator config happens to list the slow endpoint first. A
		// latency-aware gate must still choose the fast one.
		t.Setenv("WALSY_SO_ENDPOINT", slow.URL)
		t.Setenv("WALSY_SO_ENDPOINTS", slow.URL+","+fast.URL)
		soRecordEndpoint(slow.URL, 300*time.Millisecond)
		soRecordEndpoint(fast.URL, 5*time.Millisecond)
		oldEndpoint, oldClient := soEndpoint, soClient
		soEndpoint, soClient = soEndpointDefault(), &http.Client{}
		t.Cleanup(func() { soEndpoint, soClient = oldEndpoint, oldClient })

		d := askSOQ("sim state", w29_question)
		if d.Choice != "yes" {
			t.Fatalf("SO decision=%+v, want yes", d)
		}
		if gotSlow, gotFast := atomic.LoadInt32(&slowHits), atomic.LoadInt32(&fastHits); gotSlow != 0 || gotFast != 1 {
			t.Errorf("FA9 BROKEN: no latency-aware endpoint selection; slow=%d fast=%d, want slow=0 fast=1", gotSlow, gotFast)
		}
	})

	t.Run("binary_deadline", func(t *testing.T) {
		var hits int32
		postedAt := make(chan time.Time, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			postedAt <- time.Now()
			time.Sleep(250 * time.Millisecond)
			_, _ = io.WriteString(w, w29_validSO)
		}))
		defer srv.Close()
		env := w29_env(t,
			"WALSY_SO_ENDPOINT="+srv.URL,
			"EVREN_API_KEY=sim-fa-test-key",
			"WALSY_ROUTER_RETRY_BUDGET=10",
		)
		bin := w33_Build(t)
		out, err := w29_binary(t, bin, env, w29_frame(1, "binary-deadline"), 5*time.Second)
		if err != nil {
			t.Fatalf("binary run: %v; out=%s", err, out)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("binary SO posts=%d, want 1", got)
		}
		// Measure from the POST, not from process start: the deadline is about
		// how long the binary waits for SO once it has actually asked.
		if waited := time.Since(<-postedAt); waited > 220*time.Millisecond {
			t.Errorf("FA9 BROKEN: binary waited %v after SO POST for a 250ms answer; want <=220ms (~150ms deadline)", waited)
		}
	})
}

func TestW29_FA10BudgetAdmission(t *testing.T) {
	t.Run("breaker_blocks_after_failures", func(t *testing.T) {
		w29_base(t)
		t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
		t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
		routerReset()
		var hits int32
		w29_stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		})
		if d := askSO("classify this"); d.Choice != "" {
			t.Errorf("first 500 decision=%+v, want empty label", d)
		}
		if d := askSO("classify this"); d.Choice != "" {
			t.Errorf("second 500 decision=%+v, want empty label", d)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("degraded SO posts=%d, want 2", got)
		}
		if d := askSO("classify this"); d.Choice != "" || d.TimedOut {
			t.Errorf("open breaker decision=%+v, want fail-open", d)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Errorf("open breaker leaked %d SO posts", got-2)
		}
	})

	t.Run("spend_after_admit_ordering", func(t *testing.T) {
		w29_base(t)
		t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "3")
		routerReset()
		postedAt := make(chan int, 8)
		w29_stub(t, func(w http.ResponseWriter, r *http.Request) {
			postedAt <- routerBudgetLeft()
			_, _ = io.WriteString(w, w29_validSO)
		})
		before := routerBudgetLeft()
		d := newDecisionBridge()
		if _, blocked := d.enforcingGate(gateFrame("tools/call", 1)); blocked {
			t.Fatal("admitted tools/call frame was blocked")
		}
		if after := routerBudgetLeft(); after != before-1 {
			t.Errorf("budget after admit=%d, want %d", after, before-1)
		}
		select {
		case at := <-postedAt:
			if at != before {
				t.Errorf("FA10 BROKEN: token spent before SO decision; budget at POST=%d, want %d", at, before)
			}
		default:
			t.Error("no SO POST observed")
		}
	})

	t.Run("denied_frame_does_not_spend", func(t *testing.T) {
		w29_base(t)
		t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "2")
		t.Setenv("WALSY_ROUTER_DENY", "read_file")
		t.Setenv("WALSY_SO_MODE", "enforcing")
		routerReset()
		before := routerBudgetLeft()
		d := newDecisionBridge()
		resp, blocked := d.enforcingGate(gateFrame("tools/call", 1))
		if !blocked {
			t.Fatalf("denied frame not blocked: %s", resp)
		}
		if got := routerBudgetLeft(); got != before {
			t.Errorf("FA10 BROKEN: denied frame spent token %d -> %d", before, got)
		}
		if !strings.Contains(string(resp), "-32001") {
			t.Errorf("wrong refusal: %s", resp)
		}
	})

	t.Run("rolling_latency_ring_gates_admission", func(t *testing.T) {
		w29_base(t)
		t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
		t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
		routerReset()
		var hits int32
		w29_stub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			time.Sleep(120 * time.Millisecond)
			_, _ = io.WriteString(w, w29_validSO)
		})
		oldTimeout := soTimeout
		soTimeout = time.Second
		t.Cleanup(func() { soTimeout = oldTimeout })

		before := routerBudgetLeft()
		admitted := 0
		for i := 0; i < 7; i++ {
			if d := askSOQ("sim state", w29_question); d.Choice == "yes" {
				admitted++
			}
		}
		if got := routerBudgetLeft(); got != before {
			t.Errorf("FA10 BROKEN: SO latency changed budget counter %d -> %d", before, got)
		}
		if admitted > 6 {
			t.Errorf("FA10 BROKEN: no rolling latency ring; admitted %d/7 slow SO calls, want <=6", admitted)
		}
		if got := atomic.LoadInt32(&hits); got != int32(admitted) {
			t.Errorf("SO posts=%d, admitted=%d; want equal", got, admitted)
		}
	})

	t.Run("binary_budget_admission", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = io.WriteString(w, w29_validSO)
		}))
		defer srv.Close()
		// Enforcing pins the endpoint to the baked base; build a throwaway
		// binary with only that base replaced, so the localhost stub wins.
		bin := filepath.Join(t.TempDir(), "walsy-system1")
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-X walsy-system1/internal/app.soEndpointDefaultBase="+srv.URL, "-o", bin, "./cmd/walsy-system1")
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pinned test build failed: %v\n%s", err, b)
		}
		env := w29_env(t,
			"WALSY_SO_MODE=enforcing",
			"WALSY_SO_APPROVER=w29-approver",
			"WALSY_SO_ENDPOINT="+srv.URL,
			"EVREN_API_KEY=sim-fa-test-key",
			"WALSY_ROUTER_RETRY_BUDGET=1",
		)
		frames := w29_frame(1, "first") + w29_frame(2, "second")
		out, err := w29_binary(t, bin, env, frames, 5*time.Second)
		if err != nil {
			t.Fatalf("binary run: %v; out=%s", err, out)
		}
		if !strings.Contains(out, "-32001") || !strings.Contains(strings.ToLower(out), "budget") {
			t.Errorf("FA10 BROKEN: no budget refusal in binary output: %s", out)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Errorf("FA10 BROKEN: binary SO posts=%d, want 1 (second frame must fail before SO)", got)
		}
	})
}
