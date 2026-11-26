package app

// End-to-end tests that cross the MCP stdio relay, the System One decision
// boundary, the audit ledger and the receipt hash chain in one session.
// All System One traffic goes to a loopback httptest stub; no live endpoint,
// environment secret or shipped artifact is needed.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	gapE2EKey      = "gap-e2e-dummy-key-not-real"
	gapE2ESOYes    = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.96}},"usage":{}}`
	gapE2EState    = "gap-e2e-decision-state-7f31"
	gapE2EOkResult = `{"content":[{"type":"text","text":"gap-e2e-ok"}]}`
)

// gapE2ELockedBuf is a concurrency-safe clientOut: bridgeStdio has two writers.
type gapE2ELockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *gapE2ELockedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *gapE2ELockedBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type gapE2ESOStub struct {
	mu     sync.Mutex
	calls  int
	bodies []string
	keys   []string
}

func (s *gapE2ESOStub) snapshot() (int, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]string(nil), s.bodies...), append([]string(nil), s.keys...)
}

// gapE2ESetup makes every process-wide input hermetic: home, mode, approver,
// fake key, endpoint candidates, router policy, metrics and the in-memory logs.
func gapE2ESetup(t *testing.T, mode string, status int, body string) (string, *gapE2ESOStub) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", mode)
	if mode == soModeEnforcing {
		t.Setenv("WALSY_SO_APPROVER", "gap-e2e-owner")
	} else {
		t.Setenv("WALSY_SO_APPROVER", "")
	}
	t.Setenv("EVREN_API_KEY", gapE2EKey)
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("WALSY_SO_ENDPOINTS", "")
	t.Setenv("WALSY_ROUTER_DENY", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "3")
	t.Setenv("WALSY_ROUTER_MIRROR_URL", "")

	stub := &gapE2ESOStub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		stub.mu.Lock()
		stub.calls++
		stub.bodies = append(stub.bodies, string(raw))
		stub.keys = append(stub.keys, r.Header.Get("x-api-key"))
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	orig := soEndpoint
	soEndpoint = server.URL
	t.Cleanup(func() { soEndpoint = orig })
	oldMetric := metricOut
	metricOut = io.Discard
	t.Cleanup(func() { metricOut = oldMetric })
	routerReset()
	t.Cleanup(routerReset) // a refused SO opens the breaker; do not leak it into later tests
	resetLogs()
	return home, stub
}

type gapE2EPipes struct {
	clientIn       *os.File
	upstreamInR    *os.File
	upstreamOutW   *os.File
	upstreamReader *bufio.Reader
	clientOut      *gapE2ELockedBuf
	done           <-chan struct{}
}

func gapE2EStartBridge(t *testing.T) *gapE2EPipes {
	t.Helper()
	cInR, cInW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe client: %v", err)
	}
	uInR, uInW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe upstream in: %v", err)
	}
	uOutR, uOutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe upstream out: %v", err)
	}
	p := &gapE2EPipes{clientIn: cInW, upstreamInR: uInR, upstreamOutW: uOutW,
		upstreamReader: bufio.NewReader(uInR), clientOut: &gapE2ELockedBuf{}}
	done := make(chan struct{})
	p.done = done
	go func() {
		defer close(done)
		bridgeStdio(cInR, p.clientOut, uInW, uOutR)
	}()
	t.Cleanup(func() {
		_ = cInW.Close()
		_ = uOutW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("bridgeStdio did not return after EOF on both sides")
		}
		for _, f := range []*os.File{cInR, uInR, uInW, uOutR} {
			_ = f.Close()
		}
	})
	return p
}

// gapE2EFrame builds a real MCP envelope with all decision capabilities on or off.
func gapE2EFrame(id int, method, state string, caps bool) []byte {
	params := map[string]any{"name": "gap_e2e_read"}
	if state != "" {
		params["state"] = state
	}
	if caps {
		params["need_label"] = true
		params["no_scratch"] = true
		params["safe"] = true
		params["reversible"] = true
		params["authority"] = true
		params["calibrated"] = true
		params["opt_in"] = true
		params["phase"] = "before-model"
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	return append(b, '\n')
}

func gapE2EReply(id int, result string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":` + result + `}` + "\n")
}

func gapE2EReadLine(t *testing.T, f *os.File, r *bufio.Reader, timeout time.Duration) (string, error) {
	t.Helper()
	_ = f.SetReadDeadline(time.Now().Add(timeout))
	return r.ReadString('\n')
}

func gapE2EForwarded(t *testing.T, p *gapE2EPipes, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := gapE2EReadLine(t, p.upstreamInR, p.upstreamReader, 3*time.Second)
		if err != nil {
			t.Fatalf("forwarded frame %d: %v (got %q)", i+1, err, line)
		}
		out = append(out, line)
	}
	return out
}

func gapE2ENoMoreFrames(t *testing.T, p *gapE2EPipes) {
	t.Helper()
	line, err := gapE2EReadLine(t, p.upstreamInR, p.upstreamReader, 150*time.Millisecond)
	if err == nil {
		t.Fatalf("frame that should have been blocked reached upstream: %q", line)
	}
}

func gapE2EWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type gapE2EAuditRow struct {
	Event    string `json:"event"`
	Method   string `json:"method"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Decision string `json:"decision"`
	Source   string `json:"source"`
}

func gapE2EAuditRows(t *testing.T, home string) []gapE2EAuditRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	rows := []gapE2EAuditRow{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row gapE2EAuditRow
		if json.Unmarshal([]byte(line), &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func gapE2EHasAudit(rows []gapE2EAuditRow, fn func(gapE2EAuditRow) bool) bool {
	for _, row := range rows {
		if fn(row) {
			return true
		}
	}
	return false
}

func gapE2ERawLedger(t *testing.T, home string, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, routerDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// gapE2EReceipts verifies the persisted chain link-by-link, then returns lines.
func gapE2EReceipts(t *testing.T, home string) ([]receipt, []string) {
	t.Helper()
	path, err := receiptStorePath()
	if err != nil {
		t.Fatalf("receipt path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read receipt chain: %v", err)
	}
	recs := []receipt{}
	lines := []string{}
	var prev []byte
	i := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		i++
		var rec receipt
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("receipt line %d: %v", i, err)
		}
		if rec.Seq != i {
			t.Fatalf("receipt seq=%d, want %d", rec.Seq, i)
		}
		wantPrev := ""
		if i > 1 {
			wantPrev = hex.EncodeToString(prev)
		}
		if rec.Prev != wantPrev {
			t.Fatalf("receipt %d prev=%q, want %q", i, rec.Prev, wantPrev)
		}
		h := sha256.New()
		_, _ = h.Write(prev)
		_, _ = h.Write([]byte(line))
		prev = h.Sum(nil)
		recs = append(recs, rec)
		lines = append(lines, line)
	}
	if len(recs) == 0 {
		t.Fatal("empty receipt chain")
	}
	return recs, lines
}

func gapE2EVerifyEvidenceLink(t *testing.T, home string) {
	t.Helper()
	store, err := receiptStorePath()
	if err != nil {
		t.Fatalf("receipt store path: %v", err)
	}
	evidence, err := receiptPath()
	if err != nil {
		t.Fatalf("receipt evidence path: %v", err)
	}
	a, err := os.Stat(store)
	if err != nil {
		t.Fatalf("stat receipt store: %v", err)
	}
	b, err := os.Stat(evidence)
	if err != nil {
		t.Fatalf("stat receipt evidence: %v", err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("evidence/receipt.chain is not the physical chain")
	}
}

func gapE2EHealth(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return out
}

func gapE2EEndpointHost(t *testing.T, health map[string]any) string {
	t.Helper()
	host, ok := health["so_endpoint_host"].(string)
	if !ok {
		t.Fatalf("health so_endpoint_host = %v, want string", health["so_endpoint_host"])
	}
	return host
}

func gapE2EAPILogs(t *testing.T) []gapE2EAuditRow {
	t.Helper()
	rec := httptest.NewRecorder()
	NewDashboardHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs status=%d body=%s", rec.Code, rec.Body.String())
	}
	var rows []gapE2EAuditRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode /api/logs: %v", err)
	}
	return rows
}

func TestGapE2EMCPDecisionAuditReceipt(t *testing.T) {
	home, stub := gapE2ESetup(t, soModeAdvisory, http.StatusOK, gapE2ESOYes)
	p := gapE2EStartBridge(t)

	if _, err := p.clientIn.Write(gapE2EFrame(1, "initialize", "", true)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := p.clientIn.Write(gapE2EFrame(2, "tools/call", gapE2EState, true)); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	fwd := gapE2EForwarded(t, p, 2)
	if !strings.Contains(fwd[0], `"method":"initialize"`) || !strings.Contains(fwd[1], `"method":"tools/call"`) {
		t.Fatalf("wrong forwarding order: %q then %q", fwd[0], fwd[1])
	}
	if !strings.Contains(fwd[1], gapE2EState) {
		t.Fatalf("tools/call state not relayed upstream: %q", fwd[1])
	}
	if _, err := p.upstreamOutW.Write(gapE2EReply(1, `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"gap-e2e","version":"0"}}`)); err != nil {
		t.Fatalf("write init reply: %v", err)
	}
	if _, err := p.upstreamOutW.Write(gapE2EReply(2, gapE2EOkResult)); err != nil {
		t.Fatalf("write tools/call reply: %v", err)
	}
	gapE2EWaitFor(t, "relayed MCP replies", func() bool {
		s := p.clientOut.String()
		return strings.Contains(s, `"id":1`) && strings.Contains(s, `"id":2`)
	})
	got := p.clientOut.String()
	if !strings.Contains(got, `"experimental"`) {
		t.Fatalf("initialize result lost route capability: %q", got)
	}
	if !strings.Contains(got, "gap-e2e-ok") {
		t.Fatalf("tools/call result not relayed to client: %q", got)
	}

	gapE2EWaitFor(t, "loopback SO decisions", func() bool {
		calls, _, _ := stub.snapshot()
		return calls == 2
	})
	calls, bodies, keys := stub.snapshot()
	if calls != 2 || !strings.Contains(bodies[1], gapE2EState) {
		t.Fatalf("SO stub calls=%d bodies=%q", calls, bodies)
	}
	for _, key := range keys {
		if key != "" {
			t.Fatal("dummy key was sent to a loopback SO stub")
		}
	}

	// Advisory mode audits the admit through the preflight/call rows; the
	// "decision" row is emitted only on the enforcing gate path.
	gapE2EWaitFor(t, "advisory admit audit row", func() bool {
		return gapE2EHasAudit(gapE2EAuditRows(t, home), func(r gapE2EAuditRow) bool {
			return r.Event == "preflight" && r.Decision == "pin"
		})
	})
	rows := gapE2EAuditRows(t, home)
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "relay_frame" && r.Mode == "tools/call"
	}) {
		t.Fatal("audit has no client relay_frame for tools/call")
	}
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "relay_frame" && r.Mode == "response" && r.Status == "ok"
	}) {
		t.Fatal("audit has no server relay_frame response")
	}
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "call" && r.Source == "so"
	}) {
		t.Fatal("audit has no SO call row")
	}
	auditRaw := gapE2ERawLedger(t, home, routerAuditName)
	if strings.Contains(auditRaw, gapE2EState) || strings.Contains(auditRaw, gapE2EKey) {
		t.Fatal("audit leaked state or key material")
	}

	gapE2EWaitFor(t, "admitted receipt", func() bool {
		recs, _ := gapE2EReceipts(t, home)
		for _, rec := range recs {
			if rec.FrameID == "2" && rec.AdmitDeny == "admit" && rec.Decision == "pin" {
				return true
			}
		}
		return false
	})
	recs, _ := gapE2EReceipts(t, home)
	gapE2EVerifyEvidenceLink(t, home)
	seen := map[string]bool{}
	for _, rec := range recs {
		seen[rec.FrameID] = true
		if rec.ResponseID != "none" || rec.Source != "so" {
			t.Fatalf("bad receipt provenance: %+v", rec)
		}
	}
	if !seen["1"] || !seen["2"] {
		t.Fatalf("receipt chain missing MCP frame ids: %v", seen)
	}
	chainRaw := gapE2ERawLedger(t, home, "receipt.chain")
	if strings.Contains(chainRaw, gapE2EState) || strings.Contains(chainRaw, gapE2EKey) {
		t.Fatal("receipt chain leaked state or key material")
	}

	health := gapE2EHealth(t)
	if health["so_mode"] != soModeAdvisory {
		t.Fatalf("health so_mode=%v, want advisory", health["so_mode"])
	}
	sha, _ := health["receipt_sha256"].(string)
	if len(sha) != 64 {
		t.Fatalf("health receipt_sha256=%q", sha)
	}
	if !strings.Contains(gapE2EEndpointHost(t, gapE2EHealth(t)), "127.0.0.1") {
		t.Fatal("health did not expose the loopback SO endpoint host")
	}
	apiRows := gapE2EAPILogs(t)
	if !gapE2EHasAudit(apiRows, func(r gapE2EAuditRow) bool {
		return r.Event == "preflight" && r.Decision == "pin"
	}) || !gapE2EHasAudit(apiRows, func(r gapE2EAuditRow) bool {
		return r.Event == "call" && r.Source == "so"
	}) {
		t.Fatal("/api/logs did not expose the E2E admit and SO call rows")
	}
}

func TestGapE2EEnforcingCapabilityRefusal(t *testing.T) {
	home, stub := gapE2ESetup(t, soModeEnforcing, http.StatusOK, gapE2ESOYes)
	p := gapE2EStartBridge(t)

	frames := [][]byte{
		gapE2EFrame(1, "initialize", "", true),
		gapE2EFrame(2, "tools/call", gapE2EState, true),
		gapE2EFrame(3, "tools/call", gapE2EState, false),
	}
	for _, frame := range frames {
		if _, err := p.clientIn.Write(frame); err != nil {
			t.Fatalf("write client frame: %v", err)
		}
	}
	fwd := gapE2EForwarded(t, p, 2)
	if !strings.Contains(fwd[0], `"id":1`) || !strings.Contains(fwd[1], `"id":2`) {
		t.Fatalf("wrong frames forwarded: %q then %q", fwd[0], fwd[1])
	}
	gapE2ENoMoreFrames(t, p) // id=3 has no explicit decision capability
	if _, err := p.upstreamOutW.Write(gapE2EReply(1, `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"gap-e2e","version":"0"}}`)); err != nil {
		t.Fatalf("write init reply: %v", err)
	}
	if _, err := p.upstreamOutW.Write(gapE2EReply(2, gapE2EOkResult)); err != nil {
		t.Fatalf("write tools/call reply: %v", err)
	}
	gapE2EWaitFor(t, "enforcing replies and refusal", func() bool {
		s := p.clientOut.String()
		return strings.Contains(s, "explicit decision capability required") &&
			strings.Contains(s, `"id":3`) && strings.Contains(s, `"id":1`) && strings.Contains(s, `"id":2`)
	})
	got := p.clientOut.String()
	if !strings.Contains(got, `"code":-32001`) {
		t.Fatalf("refusal was not a JSON-RPC -32001 error: %q", got)
	}
	if !strings.Contains(got, "gap-e2e-ok") || !strings.Contains(got, `"id":2`) {
		t.Fatalf("admitted tools/call result missing before refusal: %q", got)
	}

	calls, bodies, _ := stub.snapshot()
	if calls != 2 || !strings.Contains(bodies[1], gapE2EState) {
		t.Fatalf("refused frame reached SO: calls=%d bodies=%q", calls, bodies)
	}
	rows := gapE2EAuditRows(t, home)
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "gate" && r.Reason == "cap-missing" && r.Method == "tools/call"
	}) {
		t.Fatal("no cap-missing gate audit row for the refused frame")
	}
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "decision" && r.Decision == "pin" && r.Method == "tools/call"
	}) {
		t.Fatal("no admitting decision audit row for the allowed frame")
	}

	gapE2EWaitFor(t, "admitted enforcing receipt", func() bool {
		recs, _ := gapE2EReceipts(t, home)
		for _, rec := range recs {
			if rec.FrameID == "2" && rec.AdmitDeny == "admit" {
				return true
			}
		}
		return false
	})
	recs, _ := gapE2EReceipts(t, home)
	for _, rec := range recs {
		if rec.FrameID == "3" {
			t.Fatal("refused frame was chainable as a decision")
		}
	}
	gapE2EVerifyEvidenceLink(t, home)
	if !strings.Contains(gapE2ERawLedger(t, home, routerAuditName), "cap-missing") {
		t.Fatal("cap-missing reason absent from audit.jsonl")
	}
	if health := gapE2EHealth(t); health["so_mode"] != soModeEnforcing {
		t.Fatalf("health so_mode=%v, want enforcing", health["so_mode"])
	}
}

func TestGapE2EEnforcingSOOutageRefusal(t *testing.T) {
	home, stub := gapE2ESetup(t, soModeEnforcing, http.StatusInternalServerError, `{}`)
	p := gapE2EStartBridge(t)

	if _, err := p.clientIn.Write(gapE2EFrame(1, "initialize", "", true)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if _, err := p.clientIn.Write(gapE2EFrame(2, "tools/call", gapE2EState, true)); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	// Both in-scope frames are SO-gated before forwarding; the 500 is a real
	// outage, so enforcing refuses both and neither reaches upstream.
	gapE2EWaitFor(t, "degraded SO refusals", func() bool {
		s := p.clientOut.String()
		return strings.Count(s, "blocked: so-degraded") == 2 &&
			strings.Contains(s, `"id":1`) && strings.Contains(s, `"id":2`)
	})
	got := p.clientOut.String()
	if strings.Count(got, `"code":-32001`) != 2 {
		t.Fatalf("degraded SO did not produce two -32001 refusals: %q", got)
	}
	gapE2ENoMoreFrames(t, p)

	calls, _, _ := stub.snapshot()
	if calls != 2 {
		t.Fatalf("SO stub calls=%d, want 2", calls)
	}
	rows := gapE2EAuditRows(t, home)
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "decision" && r.Decision == "deny" && r.Reason == "so-degraded"
	}) {
		t.Fatal("no so-degraded deny audit row")
	}
	if !gapE2EHasAudit(rows, func(r gapE2EAuditRow) bool {
		return r.Event == "call" && r.Status == "http_500"
	}) {
		t.Fatal("no http_500 SO call audit row")
	}
	gapE2EWaitFor(t, "degraded receipt", func() bool {
		recs, _ := gapE2EReceipts(t, home)
		for _, rec := range recs {
			if rec.FrameID == "2" && rec.AdmitDeny == "deny" && rec.Outcome == "so-degraded" {
				return true
			}
		}
		return false
	})
	recs, _ := gapE2EReceipts(t, home)
	gapE2EVerifyEvidenceLink(t, home)
	for _, rec := range recs {
		if rec.FrameID == "2" && rec.Decision != "deny" {
			t.Fatalf("degraded frame chained as %q", rec.Decision)
		}
	}
	health := gapE2EHealth(t)
	if health["so_mode"] != soModeEnforcing {
		t.Fatalf("health so_mode=%v, want enforcing", health["so_mode"])
	}
	if !strings.Contains(gapE2EEndpointHost(t, health), "127.0.0.1") {
		t.Fatal("health did not expose the loopback SO endpoint host")
	}
}
