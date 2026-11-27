package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fix4_05_statusServer serves one fixed status and counts probes. It never
// leaves loopback and never touches state files.
func fix4_05_statusServer(t *testing.T, status int, hits *int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func fix4_05_assertFrontendFields(t *testing.T, out ComboOutcome) {
	t.Helper()
	for i, s := range out.Steps {
		if s.Name == "" || s.Base == "" || s.Status == "" || s.Detail == "" || s.LatencyMS < 0 {
			t.Fatalf("step %d missing frontend field: %+v", i, s)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("outcome is not JSON: %v", err)
	}
	for _, key := range []string{`"winner"`, `"steps"`, `"tried"`, `"rounds"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("outcome JSON missing %s: %s", key, b)
		}
	}
}

// Partial failure must keep every attempted step visible and let the next
// configured step win; later steps must not be probed after a winner.
func TestFix405ComboPartialFailureKeepsOrder(t *testing.T) {
	resetRouterForTest(t)
	var badHits, goodHits, unusedHits int64
	bad := fix4_05_statusServer(t, http.StatusBadGateway, &badHits)
	good := fix4_05_statusServer(t, http.StatusOK, &goodHits)
	unused := fix4_05_statusServer(t, http.StatusOK, &unusedHits)
	got := RunCombo([]ComboStep{
		{Name: "bad", Base: bad.URL, Weight: 3},
		{Name: "good", Base: good.URL, Weight: 2},
		{Name: "unused", Base: unused.URL, Weight: 1},
	}, 0)
	if got.Winner != "good" || got.Rounds != 1 || len(got.Steps) != 2 {
		t.Fatalf("unexpected outcome: %+v", got)
	}
	if got.Steps[0].Name != "bad" || got.Steps[0].OK || got.Steps[0].Status != "502 Bad Gateway" ||
		got.Steps[1].Name != "good" || !got.Steps[1].OK {
		t.Fatalf("step order or status changed: %+v", got.Steps)
	}
	if atomic.LoadInt64(&badHits) != 1 || atomic.LoadInt64(&goodHits) != 1 || atomic.LoadInt64(&unusedHits) != 0 {
		t.Fatalf("probe counts bad=%d good=%d unused=%d", atomic.LoadInt64(&badHits),
			atomic.LoadInt64(&goodHits), atomic.LoadInt64(&unusedHits))
	}
	fix4_05_assertFrontendFields(t, got)
}

// One hostile or malformed response must not erase the whole result or leak
// into the frontend payload.
func TestFix405ComboOneBadStepDoesNotSpoilResult(t *testing.T) {
	resetRouterForTest(t)
	var hits int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("\x00NOT-A-CREDENTIAL"))
	}))
	t.Cleanup(bad.Close)
	good := fix4_05_statusServer(t, http.StatusCreated, nil)
	got := RunCombo([]ComboStep{
		{Name: "bad", Base: bad.URL},
		{Name: "good", Base: good.URL},
	}, 0)
	if got.Winner != "good" || len(got.Steps) != 2 || got.Steps[0].OK || !got.Steps[1].OK {
		t.Fatalf("one bad step spoiled the result: %+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "NOT-A-CREDENTIAL") {
		t.Fatalf("response body leaked into result: %s", b)
	}
	fix4_05_assertFrontendFields(t, got)
}

// A step that recovers in round 2 must still win; a failed first round must
// not hide the retry from the UI.
func TestFix405ComboRoundRecoveryIsVisible(t *testing.T) {
	resetRouterForTest(t)
	var hits int64
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(flaky.Close)
	got := RunCombo([]ComboStep{{Name: "flaky", Base: flaky.URL}}, 0)
	if got.Winner != "flaky" || got.Rounds != 2 || len(got.Steps) != 2 || got.Steps[0].OK || !got.Steps[1].OK {
		t.Fatalf("round recovery lost: %+v", got)
	}
	if atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("probe count = %d, want 2", atomic.LoadInt64(&hits))
	}
	fix4_05_assertFrontendFields(t, got)
}

// Total failure must be explicit, not a blank winner with missing rows.
func TestFix405ComboAllFailStaysExplicit(t *testing.T) {
	resetRouterForTest(t)
	var hits int64
	down := fix4_05_statusServer(t, http.StatusServiceUnavailable, &hits)
	got := RunCombo([]ComboStep{{Name: "down", Base: down.URL}}, 0)
	if got.Winner != "" || got.Rounds != comboMaxRounds || len(got.Steps) != comboMaxRounds {
		t.Fatalf("silent all-fail result: %+v", got)
	}
	for i, s := range got.Steps {
		if s.OK || s.Status != "503 Service Unavailable" || s.Detail != "non-2xx response" {
			t.Fatalf("step %d mislabeled: %+v", i, s)
		}
	}
	if atomic.LoadInt64(&hits) != int64(comboMaxRounds) {
		t.Fatalf("probe count = %d, want %d", atomic.LoadInt64(&hits), comboMaxRounds)
	}
	fix4_05_assertFrontendFields(t, got)
}

// Router breaker interaction stays visible: a skipped step is still a row,
// and no network call is made for it.
func TestFix405ComboBreakerSkipIsVisible(t *testing.T) {
	resetRouterForTest(t)
	var skippedHits, winnerHits int64
	skipped := fix4_05_statusServer(t, http.StatusOK, &skippedHits)
	winner := fix4_05_statusServer(t, http.StatusOK, &winnerHits)
	for i := 0; i < 3; i++ {
		routerRecord("skipped", false)
	}
	if !routerBreakerOpen("skipped") {
		t.Fatal("breaker did not open")
	}
	got := RunCombo([]ComboStep{
		{Name: "skipped", Base: skipped.URL},
		{Name: "winner", Base: winner.URL},
	}, 0)
	if got.Winner != "winner" || len(got.Steps) != 2 || got.Steps[0].OK ||
		got.Steps[0].Status != "circuit_open" || got.Steps[0].Detail != "recent failures" {
		t.Fatalf("breaker skip hidden: %+v", got)
	}
	if atomic.LoadInt64(&skippedHits) != 0 || atomic.LoadInt64(&winnerHits) != 1 {
		t.Fatalf("probe counts skipped=%d winner=%d", atomic.LoadInt64(&skippedHits), atomic.LoadInt64(&winnerHits))
	}
	routerRecord("skipped", true)
	if routerBreakerOpen("skipped") {
		t.Fatal("success did not close breaker")
	}
	fix4_05_assertFrontendFields(t, got)
}

// Credentials go in headers only. No result field, including base and detail,
// may carry the key into the browser JSON.
func TestFix405ComboKeyNeverReachesResult(t *testing.T) {
	resetRouterForTest(t)
	const secret = "fix405-SUPER-SECRET-KEY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("x-api-key") != secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	got := RunCombo([]ComboStep{{Name: "local/m1", Base: srv.URL, Key: secret, Model: "typesafe/jev-1.14"}}, 0)
	if got.Winner != "local/m1" || len(got.Steps) != 1 || !got.Steps[0].OK || got.Steps[0].Detail != "key accepted" {
		t.Fatalf("keyed probe result: %+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), secret) {
		t.Fatalf("key leaked into result: %s", b)
	}
	fix4_05_assertFrontendFields(t, got)
}

// A timed-out step must fail over, not stall or erase later steps.
func TestFix405ComboTimeoutFailsOver(t *testing.T) {
	resetRouterForTest(t)
	var hits int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	good := fix4_05_statusServer(t, http.StatusOK, nil)
	got := RunCombo([]ComboStep{
		{Name: "slow", Base: slow.URL},
		{Name: "good", Base: good.URL},
	}, 5*time.Millisecond)
	if got.Winner != "good" || len(got.Steps) != 2 || got.Steps[0].Status != "timeout" || !got.Steps[1].OK {
		t.Fatalf("timeout blocked failover: %+v", got)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("slow step probed %d times, want 1", atomic.LoadInt64(&hits))
	}
	fix4_05_assertFrontendFields(t, got)
}

// An empty combo is an explicit no-op: JSON steps must be [], never null, and
// round count must say zero instead of inventing a winner.
func TestFix405ComboEmptyIsExplicitNoop(t *testing.T) {
	resetRouterForTest(t)
	got := RunCombo(nil, 0)
	if got.Winner != "" || got.Tried != 0 || got.Rounds != 0 || len(got.Steps) != 0 || got.Steps == nil {
		t.Fatalf("empty combo result: %+v", got)
	}
	fix4_05_assertFrontendFields(t, got)
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"steps":[]`) {
		t.Fatalf("empty steps not serialized as array: %s", b)
	}
}
