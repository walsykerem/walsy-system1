package app

// Simulation for the combo decision path: the operator's combo is the pool
// System One is asked through. A cached decision must be served without a
// second trip through that pool, and combo save must reject unusable steps.
// Reuses the w11_ helpers from sim_n16_n17_test.go (temp HOME, SO stub,
// steady-state pin) so no live System One is ever called.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A repeated decision that hits the cache returns the stored verdict and does
// not post to the combo provider again.
func TestSimGapComboDecisionCacheHitSkipsPool(t *testing.T) {
	w11_base(t)
	srv, st := w11_stub(t, w11_reply("yes", 0.95))

	if err := saveCustomProvider("Sim Combo Cache", srv.URL+"/v1/systemone", "SIM_CACHE_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := saveCombo([]stateStep{{Provider: "Sim Combo Cache", Model: "model-combo"}}); err != nil {
		t.Fatal(err)
	}

	// The default endpoint is the only case where soPool() reads the combo;
	// point only the client at the stub, not soEndpoint itself.
	ep, cl := soEndpoint, soClient
	soEndpoint, soClient = soEndpointDefaultBase, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = ep, cl })

	d := newDecisionBridge()
	w11_primePin(d) // steady state: the first admit does not rotate the pin

	first := d.decide([]byte(w11_req))
	if first.Decision != decPin || first.Reason != "so-admit" {
		t.Fatalf("first decision = %+v, want pin/so-admit through the combo", first)
	}
	if st.count() != 1 {
		t.Fatalf("first decision posts = %d, want 1", st.count())
	}
	var wire struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(st.wire()[0]), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Model != "model-combo" {
		t.Fatalf("combo provider asked for model %q, want model-combo (provider->model order lost)", wire.Model)
	}

	second := d.decide([]byte(w11_req))
	if second.Decision != decPin || second.Reason != "cache-hit" || !second.Covered {
		t.Fatalf("second decision = %+v, want cached pin/cache-hit", second)
	}
	if st.count() != 1 {
		t.Fatalf("cache hit re-asked System One through the combo pool: posts = %d, want 1", st.count())
	}
}

// Save rejects a step with no provider and one whose model is unusable, and
// leaves the stored combo untouched.
func TestSimGapComboSaveRejectsEmptyAndInvalidStep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveCustomProvider("Sim Combo Save", "http://127.0.0.1:9999/v1/systemone", "SIM_SAVE_KEY"); err != nil {
		t.Fatal(err)
	}
	s, _ := dashboardTestServer(t)

	for _, tc := range []struct {
		name string
		step comboStepJSON
	}{
		{"empty provider", comboStepJSON{Provider: "", Model: "m"}},
		{"control-char model", comboStepJSON{Provider: "Sim Combo Save", Model: "\x01"}},
	} {
		var out map[string]any
		code := postJSONTest(t, s.URL+"/api/combo", map[string]any{"steps": []comboStepJSON{tc.step}}, &out)
		if code != http.StatusBadRequest || out["ok"] != false {
			t.Fatalf("%s: status = %d body = %v, want 400", tc.name, code, out)
		}
	}

	steps, err := loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("rejected save persisted steps: %+v", steps)
	}
}
