package app

import "testing"

// G5: enforcing mode with no classifier credential refuses the frame; advisory
// keeps its documented fail-open pass. Presence of the key is the only input.
func TestNoCredentialEnforcingRefuses(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	t.Setenv("WALSY_SO_APPROVER", "test-approver")
	routerReset()
	d := newDecisionBridge()
	if d.keySet {
		t.Fatal("test needs no credential, bridge reports a key set")
	}
	r := d.decide([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`))
	if r.Decision != decDeny {
		t.Fatalf("enforcing without a credential = %q (%s), want deny", r.Decision, r.Reason)
	}
	if r.Reason != "no-key" {
		t.Fatalf("reason = %q, want no-key", r.Reason)
	}
	if !r.Covered || !r.Abstained || r.Steer == "" {
		t.Fatalf("refusal not audit-visible: %+v", r)
	}

	t.Setenv("WALSY_SO_MODE", soModeAdvisory)
	r = d.decide([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`))
	if r.Decision != decPass || r.Reason != "no-key" || !r.Passthrough {
		t.Fatalf("advisory = %+v, want pass passthrough with reason no-key", r)
	}
}
