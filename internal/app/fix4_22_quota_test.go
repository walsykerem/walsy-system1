package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const fix422Secret = "sk-fix4-22-do-not-render"

func fix422Seed(t *testing.T, base string) {
	t.Helper()
	check(t, saveCustomProvider("QUOTA-P", base, "QUOTA_KEY") == nil, "seed provider")
	check(t, saveModels(map[string][]string{"QUOTA-P": {"m1"}}) == nil, "seed models")
	check(t, saveKey("QUOTA-P", fix422Secret) == nil, "seed key")
}

func fix422Post(t *testing.T, dashboardURL string) struct {
	Status int    `json:"status"`
	Result string `json:"result"`
	Winner string `json:"winner"`
} {
	t.Helper()
	var out struct {
		Status int    `json:"status"`
		Result string `json:"result"`
		Winner string `json:"winner"`
	}
	code := postJSONTest(t, dashboardURL+"/api/keytest", map[string]string{"provider": "QUOTA-P", "model": "m1"}, &out)
	if code != http.StatusOK {
		t.Fatalf("keytest HTTP status = %d, want 200", code)
	}
	return out
}

func fix422QuotaRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := auditLogRows()
	if err != nil {
		t.Fatal(err)
	}
	var quota []auditRow
	for _, row := range rows {
		if row.Event == "quota" {
			quota = append(quota, row)
		}
	}
	return quota
}

func fix422AssertQuotaRow(t *testing.T, rows []auditRow, outcome string) {
	t.Helper()
	for _, row := range rows {
		if row.Status == "429" && row.Reason == "retry-after" && row.Count == "0" && row.Outcome == outcome {
			return
		}
	}
	t.Fatalf("missing quota audit row outcome=%q: %+v", outcome, rows)
}

// A provider 429 with Retry-After must be backed off once and audited; the
// key remains valid, so the bounded retry reaches WINNER. Retry-After: 0 keeps
// the simulation deterministic: no wall-clock wait, no external service.
func TestFix422KeytestRetryAfterBackoffReachesWinner(t *testing.T) {
	s, _, _ := simHome(t)
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+fix422Secret {
			t.Errorf("authorization header = %q, want bearer secret", got)
		}
		if got := r.Header.Get("x-api-key"); got != fix422Secret {
			t.Errorf("x-api-key header = %q, want secret", got)
		}
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	fix422Seed(t, upstream.URL)

	out := fix422Post(t, s.URL)
	if out.Status != http.StatusOK || out.Result != "WINNER m1" || out.Winner != "m1" {
		t.Fatalf("outcome = %+v, want WINNER m1", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("probe count = %d, want one bounded retry after 429", got)
	}
	fix422AssertQuotaRow(t, fix422QuotaRows(t), "backoff")
}

// Persistent quota exhaustion stays RATE LIMITED, retries only once, and is
// audited as exhausted. The key and the Retry-After value are never echoed.
func TestFix422KeytestQuotaExhaustionIsAuditedAndBounded(t *testing.T) {
	s, _, _ := simHome(t)
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	fix422Seed(t, upstream.URL)

	out := fix422Post(t, s.URL)
	if out.Status != http.StatusTooManyRequests || out.Result != "RATE LIMITED" || out.Winner != "" {
		t.Fatalf("outcome = %+v, want RATE LIMITED and no winner", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("probe count = %d, want 1 initial + 1 bounded retry", got)
	}
	rows := fix422QuotaRows(t)
	fix422AssertQuotaRow(t, rows, "backoff")
	fix422AssertQuotaRow(t, rows, "exhausted")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	audit, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(audit), fix422Secret) || strings.Contains(string(audit), strings.TrimPrefix(fix422Secret, "sk-")) {
		t.Fatal("secret leaked into quota audit")
	}
}
