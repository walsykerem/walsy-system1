package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fix4_11: the app must expose one reusable System One request template. These
// tests post that template to a loopback httptest oracle and inspect the bytes
// that actually arrive on the wire, not a local re-marshal of the input.

func fix411State() soTemplateState {
	return soTemplateState{
		Intent:      "classify the fix4 agent 11 request template",
		Constraints: []string{"no new dependency", "do not edit gate.go"},
		Evidence:    []string{"keytest.go builds its own probe body", "gate.go builds its own POST body"},
		WorkDone:    []string{"worktree opened", "blast radius checked"},
	}
}

func fix411Wire(t *testing.T, questions map[string]soTemplateQuestion) []byte {
	t.Helper()
	t.Setenv("EVREN_API_KEY", "fix411-SENTINEL-DO-NOT-LEAK")
	body, err := soRequestTemplate("clef", fix411State(), questions)
	if err != nil {
		t.Fatalf("soRequestTemplate: %v", err)
	}
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("wire = %s %s, want POST /v1/systemone", r.Method, r.URL.Path)
		}
		captured, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"model":"clef","answers":{"q1":{"type":"choice","choice":"ship","confidence":0.95}}}`)
	}))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/systemone", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post template: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !bytes.Equal(captured, body) {
		t.Fatalf("wire body changed: got %s, built %s", captured, body)
	}
	return captured
}

type fix411EnvelopeShape struct {
	Model     string `json:"model"`
	State     string `json:"state"`
	Questions map[string]struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	} `json:"questions"`
}

func fix411Envelope(t *testing.T, body []byte) fix411EnvelopeShape {
	t.Helper()
	if !json.Valid(body) {
		t.Fatalf("wire body is not valid JSON: %s", body)
	}
	var env fix411EnvelopeShape
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("wire envelope: %v", err)
	}
	if env.Model != "clef" {
		t.Errorf("model = %q, want clef", env.Model)
	}
	// The wire state must be a string holding the JSON-encoded state object.
	var state soTemplateState
	if err := json.Unmarshal([]byte(env.State), &state); err != nil {
		t.Fatalf("wire state must be a JSON string encoding an object: %s (%v)", env.State, err)
	}
	if state.Intent == "" || len(state.Constraints) == 0 ||
		len(state.Evidence) == 0 || len(state.WorkDone) == 0 {
		t.Errorf("state missing intent/constraint/evidence/work_done: %s", body)
	}
	if want := fix411State(); state.Intent != want.Intent ||
		len(state.Constraints) != len(want.Constraints) || len(state.Evidence) != len(want.Evidence) ||
		len(state.WorkDone) != len(want.WorkDone) {
		t.Errorf("decoded state = %+v, want %+v", state, want)
	}
	// No history or tools outside state; no credential in the body at all.
	for _, leak := range []string{"fix411-SENTINEL-DO-NOT-LEAK", `"api_key"`, `"x-api-key"`,
		`"authorization"`, `"messages"`, `"tools"`, `"history"`} {
		if strings.Contains(string(body), leak) {
			t.Errorf("wire body leaked %q: %s", leak, body)
		}
	}
	return env
}

func TestFix4_11ChoiceTemplateWireShape(t *testing.T) {
	body := fix411Wire(t, map[string]soTemplateQuestion{
		"q1": {Type: "choice", Instructions: "Is the fix ready to ship?",
			Criteria: map[string]string{"ship": "all checks green", "hold": "one or more checks failed"}},
	})
	env := fix411Envelope(t, body)
	q, ok := env.Questions["q1"]
	if !ok {
		t.Fatalf("no q1 on wire: %s", body)
	}
	if q.Type != "choice" || q.Instructions == "" {
		t.Errorf("choice question = %+v", q)
	}
	var criteria map[string]string
	if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
		t.Fatalf("choice criteria must be an object: %s (%v)", q.Criteria, err)
	}
	// Mutually exclusive labels: distinct names, neither is a case-insensitive
	// prefix of the other, so one answer cannot satisfy both.
	for a := range criteria {
		for b := range criteria {
			if a != b && strings.HasPrefix(strings.ToLower(a), strings.ToLower(b)) {
				t.Errorf("labels not mutually exclusive: %q vs %q", a, b)
			}
		}
	}
	if _, ok := criteria["ship"]; !ok {
		t.Errorf("missing ship label: %s", q.Criteria)
	}
	if _, ok := criteria["hold"]; !ok {
		t.Errorf("missing hold label: %s", q.Criteria)
	}
}

func TestFix4_11ScoreTemplateCriteriaArray(t *testing.T) {
	body := fix411Wire(t, map[string]soTemplateQuestion{
		"q1": {Type: "score", Instructions: "Rate the evidence strength.",
			Criteria: []soScoreCriterion{
				{Label: "weak", Description: "marketing or inference only"},
				{Label: "moderate", Description: "one shipped implementation"},
				{Label: "strong", Description: "shipped code confirmed by two sources"},
			}},
	})
	env := fix411Envelope(t, body)
	q := env.Questions["q1"]
	if q.Type != "score" {
		t.Fatalf("score type = %q", q.Type)
	}
	var criteria []soScoreCriterion
	if err := json.Unmarshal(q.Criteria, &criteria); err != nil {
		t.Fatalf("score criteria must be an array: %s (%v)", q.Criteria, err)
	}
	if len(criteria) != 3 {
		t.Fatalf("score criteria = %d, want 3", len(criteria))
	}
	for i, c := range criteria {
		if c.Label == "" || c.Description == "" {
			t.Errorf("criteria[%d] incomplete: %+v", i, c)
		}
		for j, other := range criteria {
			if i != j && c.Label == other.Label {
				t.Errorf("duplicate score label %q", c.Label)
			}
		}
	}
}

func TestFix4_11WeakConfidenceRejected(t *testing.T) {
	if passesKeyConfRule(soDecision{Choice: "ship", Conf: 0.79}) {
		t.Error("confidence 0.79 must not count as a verdict")
	}
	if !passesKeyConfRule(soDecision{Choice: "ship", Conf: 0.95}) {
		t.Error("confidence 0.95 must count as a verdict")
	}
}

func BenchmarkSORequestTemplate(b *testing.B) {
	state := soTemplateState{Intent: "classify the request", Constraints: []string{"no new dependency"},
		Evidence: []string{"wire body captured"}, WorkDone: []string{"blast radius checked"}}
	q := map[string]soTemplateQuestion{"q1": {Type: "choice", Instructions: "ready?",
		Criteria: map[string]string{"ship": "all green", "hold": "checks failed"}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := soRequestTemplate("clef", state, q); err != nil {
			b.Fatal(err)
		}
	}
}
