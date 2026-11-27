package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fix4 slice 03: a decision suggestion becomes an autonomous action only
// after a strong, well-formed System One approval. Every fixture is loopback
// and every file lives under a per-test HOME.

func fix403Env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	t.Cleanup(routerReset)
	return home
}

func fix403Layer0() layer0 {
	return layer0{NeedLabel: true, NoScratch: true, Safe: true, Reversible: true, CleanRename: true}
}

func fix403Question() soQuestion {
	return soQuestion{Type: "choice", Instructions: "approve suggestion",
		Criteria: map[string]string{"go": "apply", "hold": "skip"}}
}

func fix403SO(t *testing.T, body string, hits *atomic.Int64, delay time.Duration) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = origEndpoint, origClient; srv.Close() })
}

func fix403Answer(answer map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"model":   "clef",
		"answers": map[string]any{"gate": answer},
	})
	return string(b)
}

func fix403Approval(conf float64, probs map[string]float64) string {
	return fix403Answer(map[string]any{
		"type": "choice", "choice": "go", "confidence": conf, "probabilities": probs,
	})
}

func fix403Audit(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("audit read: %v", err)
	}
	return string(b)
}

func fix403Count(haystack, needle string) int { return strings.Count(haystack, needle) }

func TestSimFix403ConfidenceBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf float64
		want bool
	}{
		{"799", 0.799, false},
		{"800", 0.8, true},
		{"801", 0.801, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fix403Env(t)
			var hits, acts atomic.Int64
			fix403SO(t, fix403Approval(tc.conf, map[string]float64{"go": 0.95, "hold": 0.05}), &hits, 0)
			a := &soAutonomy{}
			r := a.act(`{"item":"fix4-03"}`, fix403Question(), fix403Layer0(), "suggestion-"+tc.name,
				func() error { acts.Add(1); return nil })
			if r.Acted != tc.want {
				t.Fatalf("conf %.3f acted=%v want %v (reason %q)", tc.conf, r.Acted, tc.want, r.Reason)
			}
			if got := acts.Load(); (got == 1) != tc.want {
				t.Fatalf("conf %.3f action count = %d, want %v", tc.conf, got, tc.want)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("SO POST count = %d, want 1", got)
			}
			audit := fix403Audit(t, home)
			if got := fix403Count(audit, `"status":"done"`) > 0; got != tc.want {
				t.Fatalf("conf %.3f audit done = %v, want %v (%s)", tc.conf, got, tc.want, audit)
			}
			if strings.Contains(audit, "0.8") || strings.Contains(audit, "0.95") {
				t.Fatalf("raw confidence/probabilities leaked into audit: %s", audit)
			}
		})
	}
}

func TestSimFix403DegradedAnswersDoNotAct(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		delay time.Duration
	}{
		{"timeout-high-conf", fix403Approval(0.99, map[string]float64{"go": 0.95, "hold": 0.05}), 300 * time.Millisecond},
		{"invalid-probabilities", fix403Approval(0.99, map[string]float64{"go": 1.2, "hold": -0.2}), 0},
		{"single-probability", fix403Approval(0.99, map[string]float64{"go": 1}), 0},
		{"winner-missing", fix403Answer(map[string]any{"type": "choice", "choice": "go", "confidence": 0.99,
			"probabilities": map[string]float64{"hold": 0.95, "other": 0.05}}), 0},
		{"low-margin", fix403Approval(0.99, map[string]float64{"go": 0.51, "hold": 0.49}), 0},
		{"noul", fix403Answer(map[string]any{"type": "noul", "noul": true, "confidence": 0.99}), 0},
		{"empty-choice", fix403Answer(map[string]any{"type": "choice", "choice": "  ", "confidence": 0.99,
			"probabilities": map[string]float64{"go": 0.95, "hold": 0.05}}), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := fix403Env(t)
			var hits, acts atomic.Int64
			fix403SO(t, tc.body, &hits, tc.delay)
			a := &soAutonomy{}
			r := a.act(`{"item":"fix4-03"}`, fix403Question(), fix403Layer0(), "suggestion-"+tc.name,
				func() error { acts.Add(1); return nil })
			if r.Acted || acts.Load() != 0 {
				t.Fatalf("degraded answer acted (reason %q, actions %d)", r.Reason, acts.Load())
			}
			if r.Reason == "" {
				t.Fatal("degraded answer produced no refusal reason")
			}
			if fix403Count(fix403Audit(t, home), `"status":"done"`) != 0 {
				t.Fatal("degraded answer wrote a done audit line")
			}
		})
	}
}

func TestSimFix403Layer0BoundaryAndIdempotency(t *testing.T) {
	t.Run("destructive", func(t *testing.T) {
		home := fix403Env(t)
		var hits, acts atomic.Int64
		fix403SO(t, fix403Approval(0.99, map[string]float64{"go": 0.95, "hold": 0.05}), &hits, 0)
		f := fix403Layer0()
		f.Reversible = false
		a := &soAutonomy{}
		r := a.act(`{"item":"fix4-03"}`, fix403Question(), f, "destructive-suggestion",
			func() error { acts.Add(1); return nil })
		if r.Acted || r.Reason != "layer0-gate" || acts.Load() != 0 || hits.Load() != 0 {
			t.Fatalf("destructive boundary failed: %+v actions=%d posts=%d", r, acts.Load(), hits.Load())
		}
		if fix403Count(fix403Audit(t, home), `"status":"done"`) != 0 {
			t.Fatal("destructive boundary wrote a done audit line")
		}
	})
	t.Run("repeat", func(t *testing.T) {
		home := fix403Env(t)
		var hits, acts atomic.Int64
		fix403SO(t, fix403Approval(0.95, map[string]float64{"go": 0.95, "hold": 0.05}), &hits, 0)
		a := &soAutonomy{}
		op := func() error { acts.Add(1); return nil }
		first := a.act(`{"item":"fix4-03"}`, fix403Question(), fix403Layer0(), "one-suggestion", op)
		second := a.act(`{"item":"fix4-03"}`, fix403Question(), fix403Layer0(), "one-suggestion", op)
		if !first.Acted || second.Acted || second.Reason != "duplicate" {
			t.Fatalf("idempotency failed: first=%+v second=%+v", first, second)
		}
		if hits.Load() != 1 || acts.Load() != 1 {
			t.Fatalf("repeat posted %d times and acted %d times, want 1/1", hits.Load(), acts.Load())
		}
		audit := fix403Audit(t, home)
		if fix403Count(audit, `"status":"done"`) != 1 || fix403Count(audit, `"status":"duplicate"`) != 1 {
			t.Fatalf("idempotency audit wrong: %s", audit)
		}
		if first.ID == "" || second.ID != first.ID {
			t.Fatalf("idempotency IDs missing/unstable: %q / %q", first.ID, second.ID)
		}
	})
	t.Run("action-failure", func(t *testing.T) {
		home := fix403Env(t)
		var hits, acts atomic.Int64
		fix403SO(t, fix403Approval(0.95, map[string]float64{"go": 0.95, "hold": 0.05}), &hits, 0)
		a := &soAutonomy{}
		r := a.act(`{"item":"fix4-03"}`, fix403Question(), fix403Layer0(), "failing-suggestion",
			func() error { acts.Add(1); return os.ErrPermission })
		if r.Acted || r.Reason != "action-failed" || acts.Load() != 1 {
			t.Fatalf("failed action not reported: %+v actions=%d", r, acts.Load())
		}
		if fix403Count(fix403Audit(t, home), `"status":"failed"`) != 1 {
			t.Fatal("failed action has no failed audit line")
		}
	})
}
