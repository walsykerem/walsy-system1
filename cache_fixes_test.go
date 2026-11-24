package main

import (
	"encoding/json"
	"testing"
	"time"
)

// The cache key advertises conf>=0.8+margin>=0.10; enforce it at the store
// boundary so a weak admission can never ride a strong key on a later hit.
func TestPassesKeyConfRule(t *testing.T) {
	cases := []struct {
		name string
		d    soDecision
		want bool
	}{
		{"confident with margin", soDecision{Conf: 0.95, MarginKnown: true, Margin: 0.9}, true},
		{"confident, endpoint gave no probabilities", soDecision{Conf: 0.95}, true},
		{"boundary 0.8", soDecision{Conf: 0.8}, true},
		{"below cut", soDecision{Conf: 0.79}, false},
		{"thin margin rejected", soDecision{Conf: 0.95, MarginKnown: true, Margin: 0.05}, false},
		{"boundary margin admitted", soDecision{Conf: 0.95, MarginKnown: true, Margin: 0.1}, true},
	}
	for _, c := range cases {
		if got := passesKeyConfRule(c.d); got != c.want {
			t.Errorf("%s: passesKeyConfRule(%+v) = %v, want %v", c.name, c.d, got, c.want)
		}
	}
}

// A hot key may coast one grace window past its cell TTL, never forever.
func TestWarmFallbackBoundedByDeadline(t *testing.T) {
	c := newShardedCache()
	k := [32]byte{7}
	dec := cachedDecision{Label: "yes", Choice: "yes", ConfBucket: "0.9"}
	seed := func(deadline time.Time) {
		c.warmMu.Lock()
		c.warmHits[k] = 4
		c.warmAt[k] = time.Now()
		c.warmDec[k] = dec
		c.warmExpires[k] = deadline
		c.warmMu.Unlock()
	}
	seed(time.Now().Add(-time.Second))
	if got, hit := c.lookupForDecision(k); hit {
		t.Fatalf("expired warm fallback served %+v", got)
	}
	seed(time.Now().Add(time.Minute))
	if got, hit := c.lookupForDecision(k); !hit || got.Label != "yes" {
		t.Fatalf("fresh warm fallback = %+v hit=%v, want yes", got, hit)
	}
}

// A cached decision must still carry the typed label into tools/call, and one
// shared bridge must keep the second identical frame at zero extra SO POSTs.
func TestToolsDecideUsesCachedTypedDecision(t *testing.T) {
	d, stub := w28_env(t, w28_answer(0.95), 0)
	frame := w28_frame(1, "classify sticky request", "")
	r1 := d.decideFrame(frame)
	w28_admitted(t, r1)
	r2 := d.decideFrame(frame)
	if r2.Reason != "cache-hit" || r2.LastDecision.Choice == "" {
		t.Fatalf("cache hit lost typed decision: %s", w28_signature(r2))
	}
	text := toolsDecideWithBridge("intake.intent", "classify sticky request", "", false, d, r2.LastDecision)
	var reply map[string]any
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		t.Fatalf("reply not JSON: %q: %v", text, err)
	}
	if reply["label"] != "yes" {
		t.Fatalf("cached label = %v, want yes: %v", reply["label"], reply)
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("SO POSTs = %d, want 1 (second frame must reuse cache)", got)
	}
}
