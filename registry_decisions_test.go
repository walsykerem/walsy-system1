package main

import "testing"

// The registry is data, so the checks are: it loads, the proceed list is the
// admission rule, and a gate=false item cannot reach a model call.
func TestDecisionRegistry(t *testing.T) {
	items := loadDecisionItems()
	if len(items) != 41 {
		t.Fatalf("registry has %d items, want 41", len(items))
	}
	destructive, ok := decisionItemFor("intake.destructive")
	if !ok || !destructive.Irreversible {
		t.Fatalf("intake.destructive missing or not irreversible: %+v", destructive)
	}
	if !destructive.admits("safe") {
		t.Error(`intake.destructive must admit "safe"`)
	}
	for _, bad := range []string{"destructive", "needs_approval", "yes", ""} {
		if destructive.admits(bad) {
			t.Errorf("intake.destructive must not admit %q", bad)
		}
	}
	if q := destructive.question(); len(q.Criteria) != len(destructive.Q.Labels) || q.Type != "choice" {
		t.Errorf("question must carry the item's own labels: %+v", q)
	}
	local, ok := decisionItemFor("tools.pick")
	if !ok || local.Gate {
		t.Error("tools.pick must load as gate=false (local, no model call)")
	}
	if _, ok := decisionItemFor("no.such.item"); ok {
		t.Error("unknown id must not resolve")
	}
}

func TestRegistryItemRoutesWithoutModel(t *testing.T) {
	// gate=false must short-circuit before any HTTP: a broken endpoint here
	// would show up as a timeout, so a fast local pass proves no call happened.
	soEndpoint = "http://127.0.0.1:1/v1/systemone"
	res := route(`{"state":"decide which tool fits this step","phase":"before-model","authority":true,"opt_in":true,"calibrated":true,"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"item":"tools.pick"}`, nil, nil)
	if res.Reason != "registry-local" {
		t.Fatalf("gate=false item must pass locally, got %+v", res)
	}
}
