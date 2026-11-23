package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRegistryItemRoutesWithoutModel(t *testing.T) {
	// gate=false must short-circuit before any HTTP: a broken endpoint here
	// would show up as a timeout, so a fast local pass proves no call happened.
	soEndpoint = "http://127.0.0.1:1/v1/systemone"
	res := route(`{"state":"decide which tool fits this step","method":"tools/call","phase":"before-model","authority":true,"opt_in":true,"calibrated":true,"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"item":"tools.pick"}`, nil)
	if res.Reason != "registry-local" {
		t.Fatalf("gate=false item must pass locally, got %+v", res)
	}
}

func TestEveryRegistryItemStatesItsRequirement(t *testing.T) {
	index := loadDecisionItems()
	if len(index) == 0 {
		t.Fatal("empty registry")
	}
	for _, it := range index {
		if strings.TrimSpace(it.Requirement) == "" {
			t.Errorf("%s has no requirement line", it.ID)
		}
	}
}

func TestRegistryNonProceedCarriesItsAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"chat","confidence":0.95}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	res := route(`{"state":"classify the intent of: add a retry loop to the fetch helper","method":"tools/call","phase":"before-model","authority":true,"opt_in":true,"calibrated":true,"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"item":"intake.intent"}`, nil)
	if res.Reason != "registry-nonproceed" {
		t.Fatalf("reason = %q, want registry-nonproceed (%+v)", res.Reason, res)
	}
	if res.Steer != "hand to main LLM" {
		t.Fatalf("steer = %q, want the item's own action for the chat label", res.Steer)
	}
}

// An explicit registry item must reach the envelope only when the registry
// knows it; an unknown id is ignored rather than forwarded as a decision.
func TestBuildStateItemSelection(t *testing.T) {
	cases := []struct {
		name, state, item, wantEnvelope string
		wantItem                        bool
	}{
		{"known item honoured", `"state":"classify this request"`, "intake.intent", `"item":"intake.intent"`, true},
		{"unknown item ignored", `"state":"classify this request"`, "no.such.item", `"item":""`, false},
		{"absent item stays empty", `"state":"classify this request"`, "", `"item":""`, false},
		{"item-only request keeps item out of state", "", "tools.pick", `"item":"tools.pick"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := "{"
			if tc.state != "" {
				req += tc.state
			}
			if tc.item != "" {
				if tc.state != "" {
					req += ","
				}
				req += `"item":"` + tc.item + `"`
			}
			req += "}"
			env := buildState(req)
			if !strings.Contains(env, tc.wantEnvelope) {
				t.Fatalf("envelope = %s, want %s", env, tc.wantEnvelope)
			}
			if !tc.wantItem && strings.Contains(env, tc.item) && tc.item != "" {
				t.Fatalf("unknown item %q leaked into envelope %s", tc.item, env)
			}
			if tc.state == "" && !strings.Contains(env, `"state":"{}"`) {
				t.Fatalf("item key leaked into state: %s", env)
			}
		})
	}
}

// The relay path (not just the MCP tool) must act on a registry item: a
// gate=false item spends no model call, and one admitting label is a single
// call whose result names that label and the item's own action.
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

// specRow is one audit spec line mapped to the registry row that carries it:
// the label that must be admitted and one that must be refused.
type specRow struct {
	spec, id, admit, blocked string
}

// Every line of the 41-point audit spec that has no other owner: the mapping
// is data, so the test is the same for every row.
var specRows = []specRow{
	{"#2 request clarity", "intake.clarity", "clear", "ambiguous"},
	{"#7 reasoning start", "reasoning.start", "open", "answer_now"},
	{"#10 hypothesis generation", "reasoning.hypothesis", "grounded", "guess"},
	{"#12 stale hypothesis", "reasoning.supersede", "current", "superseded"},
	{"#16 past decision recall", "context.recall", "have", "recall"},
	{"#19 context noise", "context.filter", "clean", "noisy"},
	{"#21 attention drift", "context.alignment", "aligned", "drifted"},
	{"#24 unread write", "write.unread", "read", "unread"},
	{"#26 tool result sufficiency", "tools.result_sufficient", "sufficient", "insufficient"},
	{"#27 empty/error tool output", "tools.result_state", "usable", "error"},
	{"#29 hallucinated path", "truth.path_exists", "exists", "missing"},
	{"#31 goal drift", "reasoning.goal_drift", "on_goal", "drifted"},
	{"#32 research saturation", "retrieval.saturation", "saturated", "gaps_remain"},
	{"#33 fix->build approval", "fix.build_pass", "pass", "fail"},
	{"#34 blast radius", "impact.blast_radius", "contained", "broad"},
	{"#35 diff security", "fix.diff_security", "clean", "risky"},
	{"#36 type/interface break", "impact.break", "compatible", "breaking"},
	{"#37 build error bucket", "truth.failure_bucket", "test", "build"},
	{"#38 test root cause", "fix.root_cause", "root", "symptom"},
	{"#39 post-patch re-verify", "fix.test_pass", "pass", "fail"},
	{"#40 rollback threshold", "fix.revert", "keep", "revert"},
	{"#41 mid-task ask human", "interaction.ask_human", "proceed", "ask"},
	{"#42 output grade", "truth.output_grade", "pass", "one_more"},
	{"#42 done claim", "truth.done_claim", "verified", "unverified"},
	{"#42 anti-shirk", "antishirk", "complete", "do_more"},
}

func TestRegistrySpecRowsAreReal(t *testing.T) {
	for _, tc := range specRows {
		it, ok := decisionItemFor(tc.id)
		if !ok {
			t.Errorf("%s (%s): no registry row", tc.spec, tc.id)
			continue
		}
		if len(it.Proceed) == 0 {
			t.Errorf("%s: empty proceed list; nothing can be admitted", tc.id)
		}
		for _, p := range it.Proceed {
			if _, ok := it.Q.Labels[p]; !ok {
				t.Errorf("%s: proceed label %q is not in its own label set", tc.id, p)
			}
			if it.Action[p] == "" {
				t.Errorf("%s: proceed label %q has no action steer", tc.id, p)
			}
		}
		if !it.admits(tc.admit) {
			t.Errorf("%s: must admit %q", tc.id, tc.admit)
		}
		if it.admits(tc.blocked) {
			t.Errorf("%s: must refuse %q", tc.id, tc.blocked)
		}
		if _, ok := it.Q.Labels[tc.blocked]; !ok {
			t.Errorf("%s: refused label %q is not in its own label set", tc.id, tc.blocked)
		}
	}
}

// decideWith runs one label through the relay's MCP consult path and returns
// the parsed reply: admitted/label above the threshold, concrete steer below.
func decideWith(t *testing.T, item, label string, conf float64) decideReply {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"answers":{"gate":{"type":"choice","choice":%q,"confidence":%v}}}`, label, conf)))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()
	var reply decideReply
	if err := json.Unmarshal([]byte(toolsDecide(item, "spec row state", "before-model", false)), &reply); err != nil {
		t.Fatalf("%s: reply is not JSON: %v", item, err)
	}
	return reply
}

func TestRegistrySpecRowsAdmitAndSteer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "test-key")
	for _, tc := range specRows {
		it, ok := decisionItemFor(tc.id)
		if !ok {
			t.Errorf("%s: no registry row", tc.id)
			continue
		}
		admit := decideWith(t, tc.id, tc.admit, 0.95)
		if !admit.Admitted || admit.Label != tc.admit {
			t.Errorf("%s admit: admitted=%v label=%q, want true/%q", tc.id, admit.Admitted, admit.Label, tc.admit)
		}
		if admit.Action != it.Action[tc.admit] || admit.Steer != "" {
			t.Errorf("%s admit: action=%q steer=%q, want %q and no steer", tc.id, admit.Action, admit.Steer, it.Action[tc.admit])
		}

		refuse := decideWith(t, tc.id, tc.blocked, 0.95)
		if refuse.Admitted || refuse.Label != tc.blocked {
			t.Errorf("%s refuse: admitted=%v label=%q, want false/%q", tc.id, refuse.Admitted, refuse.Label, tc.blocked)
		}
		if refuse.Steer != it.Action[tc.blocked] {
			t.Errorf("%s refuse: steer=%q, want the item's own action %q", tc.id, refuse.Steer, it.Action[tc.blocked])
		}

		low := decideWith(t, tc.id, tc.admit, 0.7)
		if low.Admitted {
			t.Errorf("%s low conf: below-0.8 label was admitted", tc.id)
		}
		// A below-cut label is kept but not admitted (gate.go), so the consult
		// path proves non-admission here; the steer rides on the label.
	}
}

// The live sweep's two mislabels and one low-confidence row: the offline fix
// is structural (gate, tense labels, explicit required fields); whether the
// model now discriminates needs a live POST and is not asserted here.
func TestRegistryMeasuredFixes(t *testing.T) {
	priority, _ := decisionItemFor("reasoning.priority")
	if priority.Gate {
		t.Error("reasoning.priority saturated both states; want gate=false")
	}
	if !strings.Contains(priority.Requirement, "gate=false") {
		t.Error("reasoning.priority must state why it is gate=false in its requirement")
	}
	if !priority.admits("first") || !priority.admits("next") || priority.admits("defer") {
		t.Errorf("reasoning.priority proceed list wrong: %v", priority.Proceed)
	}

	destructive, _ := decisionItemFor("intake.destructive")
	if !strings.Contains(destructive.Q.Instructions, "already run") ||
		!strings.Contains(destructive.Q.Instructions, "has not started") {
		t.Error("intake.destructive instructions must ask for tense/state evidence")
	}
	if !destructive.admits("safe") || destructive.admits("destructive") || destructive.admits("needs_approval") {
		t.Errorf("intake.destructive proceed list wrong: %v", destructive.Proceed)
	}

	sufficient, _ := decisionItemFor("context.sufficient")
	for _, field := range []string{"target path", "content or schema", "command or endpoint", "permission"} {
		if !strings.Contains(sufficient.Q.Instructions, field) {
			t.Errorf("context.sufficient must name the required field %q", field)
		}
	}
	if !sufficient.admits("sufficient") || sufficient.admits("missing") {
		t.Errorf("context.sufficient proceed list wrong: %v", sufficient.Proceed)
	}
}
