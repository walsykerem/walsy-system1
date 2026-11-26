package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// assertRegistryCount56 pins the exact registry size (shared by the decision
// and sweep tests). Exact, not a floor: a dropped requirement is a data bug,
// and a new one should be a deliberate change to this count. FA11 added
// standalone model + route items next to reasoning.model_route (58).
func assertRegistryCount56(t *testing.T, items map[string]decisionItem) {
	t.Helper()
	if len(items) != 58 {
		t.Fatalf("registry has %d items, want exactly 58", len(items))
	}
}

// The registry is data, so the checks are: it loads, the proceed list is the
// admission rule, and a gate=false item cannot reach a model call.
func TestDecisionRegistry(t *testing.T) {
	items := loadDecisionItems()
	assertRegistryCount56(t, items)
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

func TestRegistryItemAdmissionTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"build","confidence":0.95}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	cases := []struct {
		name, payload                                  string
		wantDecision, wantReason, wantLabel, wantSteer string
		wantCalls                                      int
	}{
		{"gate false item spends no model call", `{"state":"decide which tool fits this step","method":"tools/call","item":"tools.pick","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`,
			decPass, "registry-local", "", "decide locally from the registry item's proceed list; no model call is needed.", 0},
		{"admitting label does not call a second model", `{"state":"classify the intent of: add a retry loop","method":"tools/call","item":"intake.intent","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`,
			decPin, "so-admit", "build", "continue", 1},
		// A label outside the generic question's set is a typed invalid-response,
		// not a generic degraded answer: the refusal reason stays precise.
		{"unknown item routes the generic question", `{"state":"classify this request","method":"tools/call","item":"no.such.item","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}`,
			decPass, "invalid-response", "", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			res := route(buildSOState(tc.payload), nil)
			if res.Decision != tc.wantDecision || res.Reason != tc.wantReason {
				t.Fatalf("result = %+v, want decision %q reason %q", res, tc.wantDecision, tc.wantReason)
			}
			if res.Label != tc.wantLabel || res.Steer != tc.wantSteer {
				t.Fatalf("label/steer = %q/%q, want %q/%q", res.Label, res.Steer, tc.wantLabel, tc.wantSteer)
			}
			if calls != tc.wantCalls {
				t.Fatalf("model calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
