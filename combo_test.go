package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestComboFirstOKWins(t *testing.T) {
	resetRouterForTest(t)
	hit := 0
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++ }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit += 10 }))
	defer b.Close()
	got := RunCombo([]ComboStep{{Name: "a", Base: a.URL}, {Name: "b", Base: b.URL}}, 0)
	if got.Winner != "a" || got.Tried != 2 || len(got.Steps) != 1 || !got.Steps[0].OK {
		t.Fatalf("bad outcome: %+v", got)
	}
	if hit != 1 {
		t.Fatalf("expected only first step called, hit=%d", hit)
	}
}

func TestComboFailThenOK(t *testing.T) {
	resetRouterForTest(t)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadGateway)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer b.Close()
	got := RunCombo([]ComboStep{{Name: "a", Base: a.URL}, {Name: "b", Base: b.URL}}, 0)
	if got.Winner != "b" || len(got.Steps) != 2 || got.Steps[0].OK || !got.Steps[1].OK {
		t.Fatalf("bad outcome: %+v", got)
	}
	if got.Steps[0].Status != "502 Bad Gateway" {
		t.Fatalf("bad status: %q", got.Steps[0].Status)
	}
}

func TestComboAllFail(t *testing.T) {
	resetRouterForTest(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusServiceUnavailable)
	})
	a, b := httptest.NewServer(h), httptest.NewServer(h)
	defer a.Close()
	defer b.Close()
	got := RunCombo([]ComboStep{{Name: "a", Base: a.URL}, {Name: "b", Base: b.URL}}, 0)
	if got.Winner != "" || got.Rounds != comboMaxRounds || len(got.Steps) != 2*comboMaxRounds {
		t.Fatalf("bad outcome: %+v", got)
	}
	for i, step := range got.Steps {
		if step.OK {
			t.Fatalf("step %d should fail: %+v", i, step)
		}
	}
}

func TestComboEmptyBase(t *testing.T) {
	resetRouterForTest(t)
	got := RunCombo([]ComboStep{{Name: "x"}}, 0)
	if len(got.Steps) != comboMaxRounds || got.Steps[0].OK || got.Steps[0].Status != "no_base" ||
		got.Steps[0].LatencyMS != 0 || got.Winner != "" || got.Rounds != comboMaxRounds {
		t.Fatalf("bad outcome: %+v", got)
	}
}

func TestComboTimeout(t *testing.T) {
	resetRouterForTest(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "slow", Base: s.URL}}, 20*time.Millisecond)
	if len(got.Steps) != comboMaxRounds || got.Steps[0].OK || got.Steps[0].Status != "timeout" || got.Winner != "" {
		t.Fatalf("bad outcome: %+v", got)
	}
}

func TestComboWeightedFailover(t *testing.T) {
	resetRouterForTest(t)
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer low.Close()
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadGateway)
	}))
	defer high.Close()
	steps := []ComboStep{
		{Name: "low", Base: low.URL, Weight: 1},
		{Name: "high", Base: high.URL, Weight: 10},
	}
	got := RunCombo(steps, 0)
	if got.Winner != "low" || len(got.Steps) != 2 || got.Steps[0].Name != "high" ||
		got.Steps[0].Status != "502 Bad Gateway" || !got.Steps[1].OK {
		t.Fatalf("weighted outcome: %+v", got)
	}
}

func TestComboNegativeWeightMeansOne(t *testing.T) {
	resetRouterForTest(t)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer b.Close()
	got := RunCombo([]ComboStep{{Name: "a", Base: a.URL, Weight: -3}, {Name: "b", Base: b.URL, Weight: 1}}, 0)
	if got.Winner != "a" || len(got.Steps) != 1 {
		t.Fatalf("tie outcome: %+v", got)
	}
}

func TestComboProbeUsesPostJSON(t *testing.T) {
	resetRouterForTest(t)
	var method, contentType, body string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, contentType = r.Method, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "x", Base: s.URL}}, 0)
	if !got.Steps[0].OK {
		t.Fatalf("probe not reachable: %+v", got)
	}
	if method != http.MethodPost || contentType != "application/json" ||
		!strings.Contains(body, `"model"`) || !strings.Contains(body, `"state"`) ||
		!strings.Contains(body, `"questions"`) || strings.Contains(body, `"messages"`) {
		t.Fatalf("probe = %s / %s / %q", method, contentType, body)
	}
}

func TestComboMethodNotAllowedIsReachable(t *testing.T) {
	resetRouterForTest(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "x", Base: s.URL}}, 0)
	if !got.Steps[0].OK || got.Winner != "x" || got.Steps[0].Detail != "endpoint reachable" {
		t.Fatalf("405 should be reachable: %+v", got)
	}
}

func TestComboNotFoundAndServerErrorFail(t *testing.T) {
	resetRouterForTest(t)
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFound.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	got := RunCombo([]ComboStep{{Name: "404", Base: notFound.URL}, {Name: "500", Base: broken.URL}}, 0)
	if got.Winner != "" || got.Rounds != comboMaxRounds || len(got.Steps) != 2*comboMaxRounds {
		t.Fatalf("404/500 should fail: %+v", got)
	}
	for _, step := range got.Steps {
		if step.Detail != "non-2xx response" {
			t.Fatalf("bad detail for %s: %q", step.Name, step.Detail)
		}
	}
}

func TestComboCircuitBreakerSkipsRecentFailure(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "1")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "5s")
	routerReset()
	hit := 0
	cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++ }))
	defer cold.Close()
	warm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer warm.Close()
	routerRecord("cold", false)
	got := RunCombo([]ComboStep{{Name: "cold", Base: cold.URL}, {Name: "warm", Base: warm.URL}}, 0)
	if got.Winner != "warm" || len(got.Steps) != 2 || got.Steps[0].Status != "circuit_open" {
		t.Fatalf("breaker outcome: %+v", got)
	}
	if hit != 0 {
		t.Fatalf("open breaker still dialed provider, hit=%d", hit)
	}
}

func TestComboSendsKeyAndRejects401(t *testing.T) {
	resetRouterForTest(t)
	var method, auth, apiKey string
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth, apiKey = r.Method, r.Header.Get("Authorization"), r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer denied.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-combo-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer allowed.Close()

	got := RunCombo([]ComboStep{
		{Name: "a/model-a", Base: denied.URL, Key: "sk-combo-1"},
		{Name: "b/model-b", Base: allowed.URL, Key: "sk-combo-2"},
	}, 0)
	if got.Winner != "b/model-b" || len(got.Steps) != 2 || got.Steps[0].OK || got.Steps[1].OK != true {
		t.Fatalf("keyed combo: %+v", got)
	}
	if got.Steps[0].Detail != "key rejected" || got.Steps[0].Status != "401 Unauthorized" {
		t.Fatalf("rejected step: %+v", got.Steps[0])
	}
	if method != http.MethodPost || auth != "Bearer sk-combo-1" || apiKey != "sk-combo-1" {
		t.Fatalf("probe request: method=%q auth=%q apiKey=%q", method, auth, apiKey)
	}
	if strings.Contains(fmt.Sprint(got), "sk-combo-1") || strings.Contains(fmt.Sprint(got), "sk-combo-2") {
		t.Fatalf("combo result leaked a key: %+v", got)
	}
}

// A non-2xx answer never proves a credential: only a 2xx does. A 422 body is
// the endpoint refusing the request, not a key that was accepted, so the step
// fails over instead of winning.
func TestComboNon2xxWithKeyDoesNotWin(t *testing.T) {
	resetRouterForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	got := RunCombo([]ComboStep{{Name: "c/m", Base: srv.URL, Key: "sk-ok"}}, 0)
	if got.Winner != "" {
		t.Fatalf("422 must not win: %+v", got)
	}
	if len(got.Steps) == 0 || got.Steps[0].OK || got.Steps[0].Detail != "non-2xx response" {
		t.Fatalf("422 with key: %+v", got)
	}
}

// The reported failure: a 400 answered to a chat-shaped request used to be
// reported as "key accepted" and won the combo on round 1. It must fail over.
func TestComboBadRequestWithKeyDoesNotWin(t *testing.T) {
	resetRouterForTest(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer bad.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	got := RunCombo([]ComboStep{
		{Name: "OpenRouter/typesafe/jev-1.14", Base: bad.URL, Key: "sk-a", Model: "typesafe/jev-1.14"},
		{Name: "Evren System One/cloudflare/clef", Base: ok.URL, Key: "sk-b", Model: "cloudflare/clef"},
	}, 0)
	if got.Winner == "" {
		t.Fatalf("combo must fail over to the working step: %+v", got)
	}
	if got.Steps[0].OK || got.Steps[0].Detail == "key accepted" {
		t.Fatalf("400 must not be reported as an accepted key: %+v", got.Steps[0])
	}
}

// The step's own model id is asked for in full: "typesafe/jev-1.14" is a
// different model than "jev-1.14".
func TestComboProbeBodyUsesStepModelField(t *testing.T) {
	resetRouterForTest(t)
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	RunCombo([]ComboStep{{Name: "OpenRouter/typesafe/jev-1.14", Base: srv.URL, Model: "typesafe/jev-1.14"}}, 0)
	if !strings.Contains(body, `"model":"typesafe/jev-1.14"`) {
		t.Fatalf("probe body = %s", body)
	}
}

func TestComboProbeBodyUsesStepModel(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	RunCombo([]ComboStep{{Name: "p/cloudflare/clef", Base: srv.URL}}, 0)
	if !strings.Contains(body, `"model":"clef"`) {
		t.Fatalf("probe body = %s", body)
	}
}

func TestRunComboWrapsToNextRound(t *testing.T) {
	resetRouterForTest(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	var hits int
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer flaky.Close()
	got := RunCombo([]ComboStep{{Name: "dead", Base: dead.URL}, {Name: "flaky", Base: flaky.URL}}, 0)
	if got.Winner != "flaky" || got.Rounds != 2 {
		t.Fatalf("bad wrap: %+v", got)
	}
	if len(got.Steps) != 4 || got.Steps[3].Name != "flaky" || !got.Steps[3].OK {
		t.Fatalf("bad step trace: %+v", got.Steps)
	}
}

func TestRunComboStopsAfterMaxRounds(t *testing.T) {
	resetRouterForTest(t)
	var hits int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "bad", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	got := RunCombo([]ComboStep{{Name: "only", Base: s.URL}}, 0)
	if got.Winner != "" || got.Rounds != comboMaxRounds || hits != comboMaxRounds {
		t.Fatalf("rounds=%d hits=%d winner=%q, want %d rounds and no extra probes", got.Rounds, hits, got.Winner, comboMaxRounds)
	}
}

func TestComboNoRejectedKeyWins(t *testing.T) {
	resetRouterForTest(t)
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reject.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	got := RunCombo([]ComboStep{
		{Name: "a", Base: reject.URL, Key: "sk-a"},
		{Name: "b", Base: ok.URL, Key: "sk-b"},
	}, 0)
	if got.Winner != "b" || got.Steps[0].OK || got.Steps[0].Detail != "key rejected" {
		t.Fatalf("rejected key must fail over: %+v", got)
	}
}
