package app

import (
	"encoding/json"
	"testing"
)

// The cache key advertises conf>=0.8+margin>=0.10; enforce it at the store
// boundary so a weak admission can never ride a strong key on a later hit.
// A hot key may coast one grace window past its cell TTL, never forever.
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

// A terminal abstain must keep its typed answer across a cache hit; otherwise
// toolsDecide sees an empty choice and reports "model unavailable".
