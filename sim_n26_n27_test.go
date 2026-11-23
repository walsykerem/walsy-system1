package main

// Hermetic runtime simulation for MANIFEST N26/N27. Every package-level helper
// in this file carries the w15_ prefix so it cannot collide with sibling sims.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// w15_explicitCaps is the full opt-in capability set the enforcing gate needs.
const w15_explicitCaps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

type w15Check struct {
	id      string
	verdict string
	detail  string
}

func w15Add(t *testing.T, dst *[]w15Check, id, verdict, detail string) {
	t.Helper()
	*dst = append(*dst, w15Check{id: id, verdict: verdict, detail: detail})
	t.Logf("%s %s: %s", verdict, id, detail)
	if verdict != "WORKS" {
		t.Errorf("%s: %s", id, detail)
	}
}

func w15Overall(items []w15Check) string {
	v := "WORKS"
	for _, c := range items {
		if c.verdict == "BROKEN" {
			return "BROKEN"
		}
		if c.verdict == "PARTIAL" {
			v = "PARTIAL"
		}
	}
	return v
}

// w15Hermetic pins HOME and all SO/budget knobs before any router state is read.
func w15Hermetic(t *testing.T, mode, budget string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", mode)
	t.Setenv("EVREN_API_KEY", "w15-sim-key")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", budget)
	routerReset()
	return home
}

type w15Stub struct {
	posts atomic.Int64
	mu    sync.Mutex
	wire  strings.Builder
}

func (s *w15Stub) bodies() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wire.String()
}

// w15SO runs a real local HTTP SO endpoint. The answer is selected by a marker
// inside the actual POST body; every body is retained for wire assertions.
func w15SO(t *testing.T, denyMarker string) *w15Stub {
	t.Helper()
	s := &w15Stub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.wire.Write(b)
		s.wire.WriteByte('\n')
		s.mu.Unlock()
		choice := "inside"
		if strings.Contains(string(b), denyMarker) {
			choice = "outside"
		}
		_, _ = fmt.Fprintf(w, `{"answers":{"gate":{"type":"choice","choice":%q,"confidence":0.95}}}`, choice)
	}))
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
	return s
}

func w15Frame(id int, state, tool, args string) []byte {
	return []byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"state":%q,"item":"intake.scope","name":%q,"arguments":%s,%s}}`,
		id, state, tool, args, w15_explicitCaps))
}

// w15Bridge drives the real stdio interceptor; each call is one live session
// from the transport's point of view.
func w15Bridge(frames ...[]byte) ([]string, []string) {
	var in bytes.Buffer
	for _, f := range frames {
		in.Write(f)
		in.WriteByte('\n')
	}
	var clientOut, upstream bytes.Buffer
	bridgeStdio(&in, &clientOut, &upstream, strings.NewReader(""))
	split := func(text string) []string {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
	return split(clientOut.String()), split(upstream.String())
}

func w15Audit(t *testing.T, home string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestSimN26 reproduces the budget bug at the transport boundary: a steer/deny
// must not spend a token, guards must not spend one either, and only admitted
// tool calls are capped.
func TestSimN26(t *testing.T) {
	home := w15Hermetic(t, "enforcing", "2")
	stub := w15SO(t, "SIM-N26-DENY")
	var out []w15Check

	deny := w15Frame(1, "SIM-N26-DENY out of authorised scope", "sim_read", `{"path":"sim.txt"}`)
	replies, forwarded := w15Bridge(deny)
	steerOK := len(replies) == 1 && len(forwarded) == 0 &&
		strings.Contains(replies[0], "BLOCK and ask the human") &&
		!strings.Contains(replies[0], "budget spent")
	if steerOK {
		w15Add(t, &out, "N26.1-refusal-steer", "WORKS", fmt.Sprintf("deny reply: %s", replies[0]))
	} else {
		w15Add(t, &out, "N26.1-refusal-steer", "BROKEN", fmt.Sprintf("deny reply = %q, want registry-nonproceed steer and no budget text", replies))
	}
	if left := routerBudgetLeft(); left == 2 {
		w15Add(t, &out, "N26.2-refusal-no-spend", "WORKS", "budget left=2 after non-admit steer")
	} else {
		w15Add(t, &out, "N26.2-refusal-no-spend", "BROKEN", fmt.Sprintf("budget left=%d after non-admit steer, want 2", left))
	}
	audit := w15Audit(t, home)
	budgetLines := 0
	for _, rec := range audit {
		if rec["event"] == "budget" {
			budgetLines++
		}
	}
	if budgetLines == 0 {
		w15Add(t, &out, "N26.3-no-exhaustion-on-steer", "WORKS", "no budget-exhausted audit after refusal")
	} else {
		w15Add(t, &out, "N26.3-no-exhaustion-on-steer", "BROKEN", fmt.Sprintf("budget audit lines=%d after refusal, want 0", budgetLines))
	}

	before := stub.posts.Load()
	a := w15Frame(2, "SIM-N26-A inside scope", "sim_read", `{"path":"sim-a.txt"}`)
	b := w15Frame(3, "SIM-N26-B inside scope", "sim_read", `{"path":"sim-b.txt"}`)
	c := w15Frame(4, "SIM-N26-C inside scope", "sim_read", `{"path":"sim-c.txt"}`)
	replies, forwarded = w15Bridge(a, b, c)
	if len(replies) != 1 || len(forwarded) != 2 {
		w15Add(t, &out, "N26.4-admit-calls", "BROKEN",
			fmt.Sprintf("client replies=%d want 1, forwarded=%d want 2; replies=%q forwarded=%q", len(replies), len(forwarded), replies, forwarded))
	} else {
		admitOK := strings.Contains(replies[0], "router tool budget spent (2 calls") &&
			strings.Contains(replies[0], "WALSY_ROUTER_RETRY_BUDGET") &&
			strings.Contains(forwarded[0], `"id":2`) && strings.Contains(forwarded[1], `"id":3`)
		if admitOK {
			w15Add(t, &out, "N26.4-admit-calls", "WORKS", "two admits forwarded, third blocked by 2-call budget with reset hint")
		} else {
			w15Add(t, &out, "N26.4-admit-calls", "BROKEN", fmt.Sprintf("replies=%q forwarded=%q", replies, forwarded))
		}
	}
	if left := routerBudgetLeft(); left == 0 {
		w15Add(t, &out, "N26.5-budget-boundary", "WORKS", "exactly two admits consumed the 2-call budget")
	} else {
		w15Add(t, &out, "N26.5-budget-boundary", "BROKEN", fmt.Sprintf("budget left=%d after three admits, want 0", left))
	}
	if got := stub.posts.Load() - before; got == 2 {
		w15Add(t, &out, "N26.6-real-SO-traffic", "WORKS",
			fmt.Sprintf("%d SO POSTs for the two admits; third had no budget and skipped SO", got))
	} else {
		w15Add(t, &out, "N26.6-real-SO-traffic", "BROKEN", fmt.Sprintf("extra SO POSTs=%d, want 2", got))
	}
	wireText := stub.bodies()
	if strings.Contains(wireText, "SIM-N26-A") && strings.Contains(wireText, "SIM-N26-B") {
		w15Add(t, &out, "N26.7-wire-evidence", "WORKS", "both admitted states reached the SO endpoint")
	} else {
		w15Add(t, &out, "N26.7-wire-evidence", "BROKEN", "admitted states missing from SO POST bodies")
	}

	// A guard refusal happens before decide(); it must not consume the budget.
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "1")
	routerReset()
	before = stub.posts.Load()
	bad := w15Frame(7, "SIM-N26-GUARD inside scope", "run_command", `{"command":"rm -rf /sim/path"}`)
	blocked, badForwarded := w15Bridge(bad)
	if len(blocked) == 1 && len(badForwarded) == 0 && strings.Contains(blocked[0], "blocked:") &&
		!strings.Contains(blocked[0], "budget spent") {
		w15Add(t, &out, "N26.8-guard-refusal", "WORKS", fmt.Sprintf("guard reply: %s", blocked[0]))
	} else {
		w15Add(t, &out, "N26.8-guard-refusal", "BROKEN", fmt.Sprintf("guard reply = %q", blocked))
	}
	if stub.posts.Load() != before {
		w15Add(t, &out, "N26.9-guard-no-SO", "BROKEN", fmt.Sprintf("SO POSTs=%d after guard refusal, want unchanged %d", stub.posts.Load(), before))
	} else {
		w15Add(t, &out, "N26.9-guard-no-SO", "WORKS", "guard refusal sent no SO POST")
	}
	if left := routerBudgetLeft(); left != 1 {
		w15Add(t, &out, "N26.10-guard-no-spend", "BROKEN", fmt.Sprintf("budget left=%d after guard refusal, want 1", left))
	} else {
		w15Add(t, &out, "N26.10-guard-no-spend", "WORKS", "guard refusal left the single token intact")
	}
	good := w15Frame(8, "SIM-N26-GOOD inside scope", "sim_read", `{"path":"sim-good.txt"}`)
	allowed, goodForwarded := w15Bridge(good)
	if len(allowed) == 0 && len(goodForwarded) == 1 && strings.Contains(goodForwarded[0], `"id":8`) && routerBudgetLeft() == 0 {
		w15Add(t, &out, "N26.11-guard-then-admit", "WORKS", "same session forwarded the next call using the intact token")
	} else {
		w15Add(t, &out, "N26.11-guard-then-admit", "BROKEN",
			fmt.Sprintf("reply=%q forwarded=%q budget_left=%d", allowed, goodForwarded, routerBudgetLeft()))
	}

	t.Logf("TestSimN26 overall: %s", w15Overall(out))
}

// TestSimN27 checks that a real SO call is tagged source=so in the audit trail
// and that local policy lines are not falsely tagged.
func TestSimN27(t *testing.T) {
	home := w15Hermetic(t, "", "3")
	stub := w15SO(t, "SIM-N27-NEVER-DENY")
	var out []w15Check

	frame := w15Frame(11, "SIM-N27 inside scope", "sim_read", `{"path":"sim-n27.txt"}`)
	w15Bridge(frame)
	if stub.posts.Load() >= 1 {
		w15Add(t, &out, "N27.1-real-SO-call", "WORKS", fmt.Sprintf("SO POSTs=%d", stub.posts.Load()))
	} else {
		w15Add(t, &out, "N27.1-real-SO-call", "BROKEN", "no SO POST observed")
	}
	wireText := stub.bodies()
	if strings.Contains(wireText, "SIM-N27") {
		w15Add(t, &out, "N27.2-wire-state", "WORKS", "sim state reached the SO endpoint")
	} else {
		w15Add(t, &out, "N27.2-wire-state", "BROKEN", "SO POST did not carry the sim state")
	}

	audit := w15Audit(t, home)
	var soCalls []map[string]any
	for _, rec := range audit {
		if rec["event"] == "call" && rec["method"] == "systemone" {
			soCalls = append(soCalls, rec)
		}
	}
	if len(soCalls) == 0 {
		w15Add(t, &out, "N27.3-SO-call-audit-present", "BROKEN", "no call/method=systemone audit record")
	} else {
		w15Add(t, &out, "N27.3-SO-call-audit-present", "WORKS", fmt.Sprintf("SO call audit records=%d", len(soCalls)))
	}
	tagged := 0
	for _, rec := range soCalls {
		if s, ok := rec["source"].(string); ok && s == "so" {
			tagged++
		}
	}
	if len(soCalls) > 0 && tagged == len(soCalls) {
		w15Add(t, &out, "N27.4-call-source-so", "WORKS", "every SO call audit record carries source=so")
	} else {
		w15Add(t, &out, "N27.4-call-source-so", "BROKEN",
			fmt.Sprintf("SO call audit records=%d with source=so=%d; call record=%v", len(soCalls), tagged, soCalls))
	}
	falselyTagged := 0
	for _, rec := range audit {
		if rec["event"] != "gate" {
			continue
		}
		if s, ok := rec["source"].(string); ok && s == "so" {
			falselyTagged++
		}
	}
	if falselyTagged == 0 {
		w15Add(t, &out, "N27.5-local-audit-not-tagged", "WORKS", "local gate audit records have no source=so")
	} else {
		w15Add(t, &out, "N27.5-local-audit-not-tagged", "BROKEN",
			fmt.Sprintf("gate audit records tagged source=so=%d, want 0", falselyTagged))
	}

	t.Logf("TestSimN27 overall: %s", w15Overall(out))
}
