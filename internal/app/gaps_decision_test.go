package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Gap tests added for the decision/gating hot paths. All helpers use the
// gapDecision prefix to stay isolated from other test files in package main.

func TestGapDecisionHoldActiveGenerationBoundary(t *testing.T) {
	d := &decisionBridge{}

	d.setHold(1, 0.8)
	if !d.holdActive() {
		t.Fatal("setHold must arm a live hold even at the 0.8 boundary")
	}
	if d.holdStrongFor(1) {
		t.Fatal("a 0.80 boundary admit must not arm the strong hysteresis hold")
	}
	if d.holdStrongFor(2) {
		t.Fatal("a hold must never leak into a different generation")
	}

	d.setHold(1, 0.81)
	if !d.holdStrongFor(1) {
		t.Fatal("confidence above 0.8 must arm the strong hold")
	}

	d.syncHold(2)
	if d.holdActive() {
		t.Fatal("syncHold on a generation change must clear the hold window")
	}
	if d.holdStrongFor(2) {
		t.Fatal("syncHold on a generation change must clear the strong hold")
	}
}

func TestGapDecisionEnforcingGateMalformedFrame(t *testing.T) {
	d := &decisionBridge{}
	out, blocked := d.enforcingGate([]byte("not json"))
	if out != nil || blocked {
		t.Fatalf("malformed frame must fail open before any gate: out=%q blocked=%v", out, blocked)
	}
}

func TestGapDecisionMirrorCanaryLoopback(t *testing.T) {
	bodyCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mirrorCanary(srv.URL, []byte(`{"state":"api_key=supersecret and hello"}`))
	select {
	case body := <-bodyCh:
		if !strings.Contains(body, `"and hello"`) {
			t.Fatalf("mirror body lost surviving text: %q", body)
		}
		if strings.Contains(body, "supersecret") {
			t.Fatalf("mirror body leaked secret: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mirror canary did not POST to loopback server")
	}
}
