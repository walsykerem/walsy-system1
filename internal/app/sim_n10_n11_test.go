package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// w09_soState is the local SO stub. fail=0 admits, fail=1 returns HTTP 500.
// posts counts real POSTs so skip paths can be proven not to call SO.
type w09_soState struct {
	fail  atomic.Int32
	posts atomic.Int32
}

// w09_caps is the full opt-in set the enforcing gate requires. Declared locally
// so this file builds in isolation from other _test.go files.
const w09_caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func w09_home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "w09-sim-key-not-a-real-key")
	return home
}

func w09_soStub(t *testing.T, s *w09_soState) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		if s.fail.Load() != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
	}))
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
	return srv
}

func w09_bridge(t *testing.T) *decisionBridge {
	t.Helper()
	routerReset()
	old := admitStats
	admitStats = &admitWindow{}
	t.Cleanup(func() {
		admitStats = old
		routerReset()
	})
	return newDecisionBridge()
}

func w09_read(home string, name string) string {
	b, err := os.ReadFile(filepath.Join(home, routerDir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

type w09_counts struct{ n, admits, blocked int }

func w09_snapshot() w09_counts {
	admitStats.mu.Lock()
	defer admitStats.mu.Unlock()
	return w09_counts{n: admitStats.n, admits: admitStats.admits, blocked: admitStats.blocked}
}

// TestSimN10_w09SkipReceipts drives real frames through the production bridge
// and checks the observable audit/receipt/counter surfaces for every N10 item.
func TestSimN10_w09SkipReceipts(t *testing.T) {
	home := w09_home(t)
	so := &w09_soState{}
	w09_soStub(t, so)
	d := w09_bridge(t)

	// 1) Out-of-scope method: decidedFrame reports, never calls SO.
	out := d.decideFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{}}` + "\n"))
	if out.Reason != "layer0-scope" {
		t.Fatalf("out-of-scope reason = %q, want layer0-scope", out.Reason)
	}

	// 2) In-scope static word: layer0 short-circuit, no SO call.
	static := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"w09tool","state":"ping",` + w09_caps + `}}` + "\n")
	before := w09_snapshot()
	got := d.decideFrame(static)
	afterStatic := w09_snapshot()
	if got.Reason != "layer0-static" {
		t.Fatalf("static reason = %q, want layer0-static", got.Reason)
	}

	// 3) In-scope but gate-rejected (safe=false): layer0-gate, no SO call.
	gate := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"w09tool","state":"classify w09 gate skip",` +
		`"need_label":true,"no_scratch":true,"safe":false,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}` + "\n")
	gotGate := d.decideFrame(gate)
	afterGate := w09_snapshot()
	if gotGate.Reason != "layer0-gate" {
		t.Fatalf("gate reason = %q, want layer0-gate", gotGate.Reason)
	}
	if n := so.posts.Load(); n != 0 {
		t.Fatalf("skip paths made %d SO POSTs, want 0", n)
	}

	audit := w09_read(home, routerAuditName)
	chain := ""
	if p, err := receiptPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			chain = string(b)
		}
	}

	t.Run("named_out_of_scope_skip", func(t *testing.T) {
		if strings.Contains(audit, `"reason":"layer0-scope"`) {
			t.Logf("WORKS: out-of-scope skip named layer0-scope in %s", filepath.Join(routerDir, routerAuditName))
			return
		}
		t.Errorf("BROKEN: no named layer0-scope skip record; audit=%q", audit)
	})

	t.Run("named_static_skip", func(t *testing.T) {
		if strings.Contains(audit, `"reason":"layer0-static"`) {
			t.Logf("WORKS: static skip named layer0-static in %s", filepath.Join(routerDir, routerAuditName))
			return
		}
		t.Errorf("BROKEN: no named layer0-static skip record; audit=%q", audit)
	})

	t.Run("named_gate_skip", func(t *testing.T) {
		if strings.Contains(audit, `"reason":"layer0-gate"`) {
			t.Logf("WORKS: gate skip named layer0-gate in %s", filepath.Join(routerDir, routerAuditName))
			return
		}
		t.Errorf("BROKEN: layer0-gate skip not named; audit=%q", audit)
	})

	t.Run("skip_receipt_chain", func(t *testing.T) {
		if strings.Contains(chain, `"reason":"layer0-`) {
			t.Logf("WORKS: named skip receipt present in receipt.chain")
			return
		}
		t.Errorf("BROKEN: receipt.chain has no named layer0 skip receipt; chain=%q", chain)
	})

	t.Run("skips_not_counted_as_decisions", func(t *testing.T) {
		if afterGate.n == before.n && afterStatic.n == before.n {
			t.Logf("WORKS: admitStats unchanged across 2 skips: %+v", before)
			return
		}
		t.Errorf("BROKEN: skips counted as decisions; before=%+v afterStatic=%+v afterGate=%+v",
			before, afterStatic, afterGate)
	})

	t.Run("false_negative_skip_vs_admitted_corpus", func(t *testing.T) {
		for _, token := range []string{"false-negative", "skip-audit", "skip_corpus", "admitted-corpus"} {
			if strings.Contains(strings.ToLower(audit), token) {
				t.Logf("WORKS: skip-vs-corpus audit token %q present", token)
				return
			}
		}
		t.Errorf("BROKEN: no false-negative skip vs admitted corpus audit; audit=%q", audit)
	})
}

// TestSimN11_w09OperatorFeedback drives admitted, degraded and -32001 denial
// flows, then checks the real dashboard/health/tool surfaces for N11 feedback.
func TestSimN11_w09OperatorFeedback(t *testing.T) {
	w09_home(t)
	so := &w09_soState{}
	w09_soStub(t, so)
	d := w09_bridge(t)

	admit := d.decideFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"w09tool","state":"classify w09 admit alpha",` + w09_caps + `}}` + "\n"))
	if admit.Reason != "so-admit" {
		t.Fatalf("admit flow reason = %q, want so-admit", admit.Reason)
	}
	so.fail.Store(1)
	degraded := d.decideFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"w09tool","state":"classify w09 degraded beta",` + w09_caps + `}}` + "\n"))
	if degraded.Reason != "so-degraded" {
		t.Fatalf("degraded flow reason = %q, want so-degraded", degraded.Reason)
	}
	so.fail.Store(0)

	// Real enforcing denial through the production MCP HTTP transport.
	t.Setenv("WALSY_SO_MODE", "enforcing")
	mcp := httptest.NewServer(mcpHTTPHandler())
	denyFrame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"w09tool","state":"classify w09 deny"}}`)
	resp, err := http.Post(mcp.URL, "application/json", bytes.NewReader(denyFrame))
	if err != nil {
		mcp.Close()
		t.Fatalf("MCP POST: %v", err)
	}
	denyBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mcp.Close()
	if !bytes.Contains(denyBody, []byte(`"code":-32001`)) {
		t.Fatalf("enforcing denial body = %q, want JSON-RPC -32001", denyBody)
	}

	t.Setenv("WALSY_SO_MODE", "")
	dash := httptest.NewServer(NewDashboardHandler())
	t.Cleanup(dash.Close)
	c := dash.Client()
	hs, page := w09_get(t, c, dash.URL+"/")
	if hs != http.StatusOK {
		t.Fatalf("GET / = %d", hs)
	}
	zs, healthBody := w09_get(t, c, dash.URL+"/api/health")
	if zs != http.StatusOK {
		t.Fatalf("GET /api/health = %d", zs)
	}
	var health map[string]any
	if err := json.Unmarshal([]byte(healthBody), &health); err != nil {
		t.Fatalf("/api/health not JSON: %v %s", err, healthBody)
	}

	// Mutation-token probe: the read-only status strip stays and every mutation route rejects the token.
	rejects := map[string]string{}
	for _, path := range []string{"/api/feedback", "/ui/feedback", "/api/feedback/token", "/api/operator/feedback"} {
		st, body := w09_postJSON(t, c, dash.URL+path, `{"token":"w09-MUTATE","value":1}`)
		rejects[path] = http.StatusText(st) + " " + body
	}

	t.Run("read_only_mode_banner", func(t *testing.T) {
		lower := strings.ToLower(page)
		for _, marker := range []string{"salt-okunur", "read-only", "read only"} {
			if strings.Contains(lower, marker) {
				t.Logf("WORKS: mode banner marker %q served", marker)
				return
			}
		}
		t.Errorf("BROKEN: no SALT-OKUNUR/read-only mode banner in dashboard home")
	})

	t.Run("decision_and_fail_open_counters", func(t *testing.T) {
		for _, key := range []string{"decisions", "fail_open", "fail-open", "decision_count", "fail_open_count"} {
			if _, ok := health[key]; ok {
				t.Logf("WORKS: counter key %q in /api/health", key)
				return
			}
			if strings.Contains(strings.ToLower(page), key) {
				t.Logf("WORKS: counter marker %q in dashboard home", key)
				return
			}
		}
		t.Errorf("BROKEN: no decision/fail-open counters observed; health=%s", healthBody)
	})

	t.Run("so_health_widget", func(t *testing.T) {
		for _, marker := range []string{"so_health", "so-health", "so health", "systemone_health"} {
			if strings.Contains(strings.ToLower(page), marker) || strings.Contains(strings.ToLower(healthBody), marker) {
				t.Logf("WORKS: SO health marker %q observed", marker)
				return
			}
		}
		t.Errorf("BROKEN: no SO health widget observed; health=%s", healthBody)
	})

	t.Run("denial_alarm_32001", func(t *testing.T) {
		if strings.Contains(page, "-32001") && strings.Contains(strings.ToLower(page), "denial") {
			t.Logf("WORKS: -32001 denial alarm observed in dashboard home")
			return
		}
		t.Errorf("BROKEN: real -32001 denial produced but no alarm in dashboard home")
	})

	t.Run("degraded_indicator", func(t *testing.T) {
		if strings.Contains(strings.ToLower(page), "degraded") || strings.Contains(strings.ToLower(healthBody), "degraded") {
			t.Logf("WORKS: degraded indicator observed")
			return
		}
		t.Errorf("BROKEN: real so-degraded decision produced but no degraded indicator served")
	})

	t.Run("mutation_token_reject", func(t *testing.T) {
		var forms strings.Builder
		for path, got := range rejects {
			forms.WriteString(path + "=" + got + "; ")
		}
		if strings.Contains(strings.ToLower(page), "mutation_token") || strings.Contains(strings.ToLower(page), "mutation token") {
			for path, got := range rejects {
				if strings.Contains(strings.ToUpper(got), "REJECT") {
					t.Logf("WORKS: mutation token rejected at %s: %s", path, got)
					return
				}
			}
			t.Errorf("BROKEN: mutation token input served but no REJECT response; %s", forms.String())
			return
		}
		t.Errorf("BROKEN: no mutation-token status marker served; %s", forms.String())
	})
}

func w09_get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	r, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func w09_postJSON(t *testing.T, c *http.Client, url, body string) (int, string) {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}
