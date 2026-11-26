package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestYC5WholeAnswerValidation: type=="choice" and the whole answer shape are
// required. No plausible choice from a malformed response is ever taken.
func TestYC5WholeAnswerValidation(t *testing.T) {
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"valid", `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.75}}}`, "yes"},
		{"tie-kept", `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.5}}}`, "no"},
		{"missing-type", `{"answers":{"gate":{"choice":"yes","confidence":0.95}}}`, ""},
		{"wrong-type", `{"answers":{"gate":{"type":"score","choice":"yes","confidence":0.95}}}`, ""},
		{"bad-label", `{"answers":{"gate":{"type":"choice","choice":"maybe","confidence":0.9}}}`, ""},
		{"bad-confidence", `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":1.4}}}`, ""},
		{"duplicate-key", `{"answers":{"gate":{"type":"choice","choice":"yes","choice":"no","confidence":0.9}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSO(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			d := askSOQ("decide: classify this", q)
			if d.Choice != tc.want {
				t.Fatalf("choice=%q want %q", d.Choice, tc.want)
			}
		})
	}
}

// TestRankingTraceSeparate: ranking trace JSON carries reason codes only.
// Confidence and raw probabilities never appear in the separate record.
func TestRankingTraceSeparate(t *testing.T) {
	d := soDecision{Choice: "yes", Probabilities: map[string]float64{"yes": 0.9, "no": 0.1}}
	tr := rankingTrace(d, "high")
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"0.9", "0.1", "probabilities"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("ranking trace leaked %q: %s", bad, b)
		}
	}
	if !tr.Comparable || tr.Winner != "yes" || tr.RunnerUp != "no" {
		t.Fatalf("bad trace: %+v", tr)
	}
	incomparable := rankingTrace(soDecision{Choice: "yes"}, "high")
	if incomparable.Comparable || incomparable.Reason != "insufficient-distribution" {
		t.Fatalf("fail-closed trace expected: %+v", incomparable)
	}
}

// TestAdmitRateWindow: in-memory window admits only coarse status/count keys.
func TestAdmitRateWindow(t *testing.T) {
	resetRouterForTest(t)
	w := &admitWindow{}
	for i := 0; i < 100; i++ {
		w.record(i%4 != 0)
	}
	if w.n != 100 || w.admits != 75 || w.blocked != 25 {
		t.Fatalf("bad window: n=%d admits=%d blocked=%d", w.n, w.admits, w.blocked)
	}
	line := readRouterAudit(t)
	if !strings.Contains(line, `"event":"admit_rate"`) ||
		!strings.Contains(line, `"count":"75"`) ||
		!strings.Contains(line, `"status":"window100"`) {
		t.Fatalf("admit_rate audit line: %s", line)
	}
	for _, bad := range []string{"confidence", "probabilities", "\"choice\""} {
		if strings.Contains(line, bad) {
			t.Fatalf("admit_rate leaked %q: %s", bad, line)
		}
	}
}

// TestYD2EngineGenerationGuard: a rotated or swapped-out pin cannot read an
// admit cached by the previous engine. Generation is identity, not a TTL.
func TestYD2EngineGenerationGuard(t *testing.T) {
	calls := 0
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	})
	d := newDecisionBridge()
	d.keySet = true
	// Pin the engine first: the first admit otherwise pins it mid-flight,
	// which correctly prevents caching an answer computed by the old identity.
	d.pin.markSuccess(d.pin.current().Name)
	req := []byte(`{"state":"decide: engine generation test","method":"initialize",` + explicitCaps + `}`)
	if r := d.decide(req); r.Decision != decPin {
		t.Fatalf("first call not admitted: %+v", r)
	}
	if r := d.decide(req); r.Decision != decPin {
		t.Fatalf("cached call not admitted: %+v", r)
	}
	if calls != 1 {
		t.Fatalf("cache hit expected: SO calls=%d want 1", calls)
	}
	gen := d.pin.generation()
	d.pin.rotateOnFailure()
	if d.pin.generation() == gen {
		t.Fatal("rotateOnFailure did not bump generation")
	}
	if r := d.decide(req); r.Decision != decPin {
		t.Fatalf("post-rotation call not admitted: %+v", r)
	}
	if calls != 2 {
		t.Fatalf("stale cache served after rotation: SO calls=%d want 2", calls)
	}
	gen = d.pin.generation()
	d.pin.markSuccess("engine-B")
	if d.pin.generation() == gen {
		t.Fatal("markSuccess with new name did not bump generation")
	}
	if r := d.decide(req); r.Decision != decPin {
		t.Fatalf("post-swap call not admitted: %+v", r)
	}
	if calls != 3 {
		t.Fatalf("stale cache served after engine swap: SO calls=%d want 3", calls)
	}
}

func readRouterAudit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
