package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Simulation of MANIFEST N8/N9 against the real relay code. Every check drives
// a real code path (stdio transport, decision hub, SO HTTP stub, receipt file,
// shipped binary). A check fails when the requested behaviour is absent, so a
// red test is evidence of a gap, not a broken harness.

type n89Check struct {
	id      string
	verdict string
	detail  string
}

func n89Add(t *testing.T, out *[]n89Check, id, verdict, detail string) {
	t.Helper()
	*out = append(*out, n89Check{id, verdict, detail})
	t.Logf("%s: %s - %s", id, verdict, detail)
	if verdict != "WORKS" {
		t.Errorf("%s %s: %s", id, verdict, detail)
	}
}

func n89Overall(out []n89Check) string {
	v := "WORKS"
	for _, c := range out {
		if c.verdict == "BROKEN" {
			return "BROKEN"
		}
		if c.verdict == "PARTIAL" {
			v = "PARTIAL"
		}
	}
	return v
}

type n89SO struct {
	server *httptest.Server
	calls  atomic.Int64
	fail   atomic.Bool
	mu     sync.Mutex
	states []string
	keys   []string
}

func n89StartSO(t *testing.T, fail bool) *n89SO {
	t.Helper()
	s := &n89SO{}
	s.fail.Store(fail)
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.states = append(s.states, req.State)
		s.keys = append(s.keys, r.Header.Get("x-api-key"))
		s.mu.Unlock()
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("simulated SO failure"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}},"usage":{}}`))
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *n89SO) pointAt(t *testing.T) {
	t.Helper()
	old := soEndpoint
	soEndpoint = s.server.URL
	t.Cleanup(func() { soEndpoint = old })
	t.Setenv("WALSY_SO_ENDPOINT", s.server.URL)
	t.Setenv("EVREN_API_KEY", "n89-sim-key")
}

func n89Frame(id int, state string) []byte {
	params := map[string]any{
		"name":       "sim_read",
		"arguments":  map[string]any{"path": "sim.txt"},
		"state":      state,
		"need_label": true,
		"no_scratch": true,
		"safe":       true,
		"reversible": true,
		"opt_in":     true,
		"authority":  true,
		"calibrated": true,
		"phase":      "before-model",
	}
	env := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params}
	b, _ := json.Marshal(env)
	return append(b, '\n')
}

func n89ReceiptLines(t *testing.T, home string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	if os.IsNotExist(err) {
		return nil // absence is itself observable evidence, not a harness error
	}
	if err != nil {
		t.Fatalf("receipt.chain unreadable: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		t.Fatal("receipt.chain empty")
	}
	return lines
}

func n89ReceiptMap(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("receipt line is not JSON: %v", err)
	}
	return m
}

func n89Has(m map[string]any, names ...string) bool {
	for _, n := range names {
		if _, ok := m[n]; ok {
			return true
		}
	}
	return false
}

func n89QuietMetrics(t *testing.T) {
	t.Helper()
	old := metricOut
	metricOut = io.Discard
	t.Cleanup(func() { metricOut = old })
}

func n89RunBinary(t *testing.T, home, mode string) (int, string) {
	t.Helper()
	bin := w33_Build(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := []string{"HOME=" + home}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "PATH=") ||
			strings.HasPrefix(kv, "WALSY_") || strings.HasPrefix(kv, "EVREN_API_KEY=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "WALSY_SO_MODE="+mode)
	cmd := exec.CommandContext(ctx, bin, "-transport", "tools")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("binary did not exit within 5s (mode %q)", mode)
	}
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("binary launch failed (mode %q): %v", mode, err)
	return -1, ""
}

// TestSimN8 simulates the three N8 requirements: enforcing fail-closed abstain
// on SO error, a signed TTL-bound advisory exception, and startup refusal when
// no mode is explicitly configured.
func TestSimN8(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	n89QuietMetrics(t)
	routerReset()
	t.Cleanup(routerReset)
	so := n89StartSO(t, true)
	so.pointAt(t)
	var out []n89Check

	// N8.1: real stdio bridge, enforcing mode, SO answers HTTP 500.
	t.Setenv("WALSY_SO_MODE", "enforcing")
	frame := n89Frame(1, "decide: should this read proceed")
	var upstream, client bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &client, &upstream, strings.NewReader(""))
	if so.calls.Load() == 0 {
		t.Fatal("SO stub received no POST: stdio simulation did not reach the real gate")
	}
	forwarded := bytes.Contains(upstream.Bytes(), []byte(`"method":"tools/call"`))
	reply := client.String()
	blocked := strings.Contains(reply, `"code":-32001`) && strings.Contains(reply, "so-degraded")
	abstainRecorded := false
	for _, line := range n89ReceiptLines(t, home) {
		if n89ReceiptMap(t, line)["reason"] == "so-degraded" {
			abstainRecorded = true
		}
	}
	switch {
	case blocked && !forwarded && abstainRecorded:
		n89Add(t, &out, "N8.1", "WORKS", "enforcing SO 500 -> -32001 reason-coded refusal, frame not forwarded, abstain receipted")
	case blocked || abstainRecorded:
		n89Add(t, &out, "N8.1", "PARTIAL", fmt.Sprintf("SO 500 forwarded=%v, -32001 refusal=%v, abstain receipt=%v", forwarded, blocked, abstainRecorded))
	default:
		n89Add(t, &out, "N8.1", "BROKEN", fmt.Sprintf("enforcing SO 500 forwarded the frame (forwarded=%v, refusal=%v, abstain receipt=%v): no fail-closed abstain", forwarded, blocked, abstainRecorded))
	}

	// N8.2: advisory exception must be signed and TTL-bound on the ledger.
	t.Setenv("WALSY_SO_MODE", "advisory")
	d := newDecisionBridge()
	r := d.decideFrame(n89Frame(2, "decide: advisory exception audit"))
	if r.Reason != "so-degraded" || !r.Abstained || r.Decision != decPass {
		t.Fatalf("advisory SO failure simulation invalid: %+v", r)
	}
	recs := n89ReceiptLines(t, home)
	var last map[string]any
	if len(recs) > 0 {
		last = n89ReceiptMap(t, recs[len(recs)-1])
	}
	signed := n89Has(last, "signature", "sig", "hmac", "sign")
	ttl := n89Has(last, "ttl", "expires", "expires_at", "expiry", "valid_until", "exp")
	switch {
	case signed && ttl:
		n89Add(t, &out, "N8.2", "WORKS", "advisory fail-open exception carries signature and TTL")
	case len(recs) > 0:
		n89Add(t, &out, "N8.2", "PARTIAL", fmt.Sprintf("advisory fail-open receipted but signed=%v ttl=%v; keys=%v", signed, ttl, receiptKeys(last)))
	default:
		n89Add(t, &out, "N8.2", "BROKEN", "advisory SO failure is fail-open but no exception receipt exists: no signature and no TTL")
	}

	// N8.3: freshly built binary, no mode set anywhere.
	childHome := t.TempDir()
	rcUnset, msgUnset := n89RunBinary(t, childHome, "")
	rcSet, _ := n89RunBinary(t, childHome, "advisory")
	if rcSet != 0 {
		t.Fatalf("binary rejected an explicit advisory mode: rc=%d", rcSet)
	}
	if rcUnset != 0 && strings.Contains(strings.ToLower(msgUnset), "mode") {
		n89Add(t, &out, "N8.3", "WORKS", "mode unset -> startup refused")
	} else {
		n89Add(t, &out, "N8.3", "BROKEN", fmt.Sprintf("mode unset -> binary exited rc=%d (explicit mode rc=%d); no startup refusal", rcUnset, rcSet))
	}

	t.Logf("N8 OVERALL: %s", n89Overall(out))
}

func receiptKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestSimN9 simulates receipt completeness, per-request emission, and the
// sha256 hash chain over the real receipt file produced by real decisions.
func TestSimN9(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "advisory")
	n89QuietMetrics(t)
	routerReset()
	t.Cleanup(routerReset)
	so := n89StartSO(t, false)
	so.pointAt(t)

	d := newDecisionBridge()
	r1 := d.decideFrame(n89Frame(11, "decide: sim alpha"))
	r2 := d.decideFrame(n89Frame(12, "decide: sim beta"))
	if r1.Decision != decPin || r2.Decision != decPin || so.calls.Load() != 2 {
		t.Fatalf("admit simulation invalid: r1=%+v r2=%+v SO calls=%d", r1, r2, so.calls.Load())
	}
	lines := n89ReceiptLines(t, home)
	var out []n89Check

	// N9.1: required per-request redacted fields.
	m := n89ReceiptMap(t, lines[0])
	type field struct {
		name string
		keys []string
	}
	fields := []field{
		{"gateKey", []string{"gate_key", "gateKey"}},
		{"item", []string{"item"}},
		{"phase", []string{"phase"}},
		{"choice", []string{"choice"}},
		{"confBucket", []string{"conf_bucket", "confBucket", "bucket"}},
		{"admitDeny", []string{"admit", "deny", "admit_deny"}},
		{"outcome", []string{"outcome"}},
		{"reason", []string{"reason"}},
		{"latency", []string{"latency_ms", "latency"}},
		{"responseId", []string{"response_id"}},
		{"ts", []string{"ts", "timestamp"}},
	}
	var missing []string
	for _, f := range fields {
		if !n89Has(m, f.keys...) {
			missing = append(missing, f.name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	if err != nil {
		t.Fatal(err)
	}
	leaky := bytes.Contains(raw, []byte("n89-sim-key")) || bytes.Contains(raw, []byte("0.95"))
	if leaky {
		n89Add(t, &out, "N9.1", "BROKEN", "receipt leaked the simulated key or raw confidence")
	} else if len(missing) == 0 {
		n89Add(t, &out, "N9.1", "WORKS", "all required redacted receipt fields present")
	} else if len(missing) < len(fields) {
		n89Add(t, &out, "N9.1", "PARTIAL", "missing fields: "+strings.Join(missing, ","))
	} else {
		n89Add(t, &out, "N9.1", "BROKEN", "no required receipt fields found")
	}
	if m["response_id"] != "none" {
		t.Errorf("N9.1: response_id = %v, want honest literal none", m["response_id"])
	}

	// N9.2: recompute H(n)=sha256(H(n-1)||canonical JSON line) independently.
	dirSt, err := os.Stat(filepath.Join(home, routerDir))
	if err != nil || dirSt.Mode().Perm() != 0700 {
		t.Errorf("N9.2: receipt dir perm/state invalid: %v %v", dirSt, err)
	}
	fileSt, err := os.Stat(filepath.Join(home, routerDir, "receipt.chain"))
	if err != nil || fileSt.Mode().Perm() != 0600 {
		t.Errorf("N9.2: receipt file perm/state invalid: %v %v", fileSt, err)
	}
	prev := []byte{}
	chainOK := true
	for i, line := range lines {
		cur := n89ReceiptMap(t, line)
		if cur["seq"] != float64(i+1) || cur["prev"] != hex.EncodeToString(prev) {
			chainOK = false
		}
		h := sha256.New()
		_, _ = h.Write(prev)
		_, _ = h.Write(line)
		sum := h.Sum(nil)
		prev = sum
	}
	if len(lines) < 2 {
		chainOK = false
	}
	tampered := append([]byte(nil), lines[0]...)
	tampered[bytes.Index(tampered, []byte(`"decision"`))+11] = 'X'
	tamperedHead := sha256.Sum256(tampered) // prev is empty for line 1
	// A changed first line must not reproduce the second line's prev.
	tamperDetected := hex.EncodeToString(tamperedHead[:]) != n89ReceiptMap(t, lines[1])["prev"]
	if chainOK && tamperDetected {
		n89Add(t, &out, "N9.2", "WORKS", "seq/prev/nonce chain recomputes; edited first line breaks the next prev")
	} else {
		n89Add(t, &out, "N9.2", "BROKEN", fmt.Sprintf("chain recompute ok=%v tamper detected=%v", chainOK, tamperDetected))
	}

	// N9.3: per-request emission with distinct nonces.
	n1 := n89ReceiptMap(t, lines[0])["nonce"]
	n2 := n89ReceiptMap(t, lines[1])["nonce"]
	if len(lines) == 2 && n1 != "" && n1 != n2 {
		n89Add(t, &out, "N9.3", "WORKS", "two in-scope frames -> two receipts with distinct nonces")
	} else {
		n89Add(t, &out, "N9.3", "PARTIAL", fmt.Sprintf("receipts=%d nonce1=%v nonce2=%v", len(lines), n1, n2))
	}

	t.Logf("N9 OVERALL: %s", n89Overall(out))
}
