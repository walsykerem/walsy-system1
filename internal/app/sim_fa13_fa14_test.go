package app

// Genuine simulation for MANIFEST FA13 and FA14.
// Every check drives real code: the shipped HTTP handler, the real decision
// bridge, an httptest System One stub, the real receipt chain, the real audit
// writer, the real dashboard handler and the shipped binary. Each assertion
// states the REQUIRED behavior, so a missing feature fails here and a red test
// is evidence of a gap, not a broken harness. No production file is touched.

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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type faCheck struct {
	id      string
	verdict string
	detail  string
}

func faAdd(t *testing.T, out *[]faCheck, id, verdict, detail string) {
	t.Helper()
	*out = append(*out, faCheck{id, verdict, detail})
	t.Logf("%s: %s - %s", id, verdict, detail)
	if verdict != "WORKS" {
		t.Errorf("%s %s: %s", id, verdict, detail)
	}
}

func faOverall(out []faCheck) string {
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

func faGrade(found, total int) string {
	if found == total {
		return "WORKS"
	}
	if found > 0 {
		return "PARTIAL"
	}
	return "BROKEN"
}

// fa1314Env makes HOME hermetic. WALSY_SO_MODE is set explicitly by the caller so
// no inherited config can decide the outcome; "" clears it where needed.
func fa1314Env(t *testing.T, mode string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", mode)
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "fa-sim-key") // fake; never read from disk or printed
	routerReset()
	t.Cleanup(routerReset)
	resetLogs()
	t.Cleanup(resetLogs)
	return home
}

func faCaptureMetrics(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := metricOut
	buf := &bytes.Buffer{}
	metricOut = buf
	t.Cleanup(func() { metricOut = old })
	return buf
}

type faStub struct {
	srv   *httptest.Server
	calls atomic.Int64
}

func faStartSO(t *testing.T) *faStub {
	t.Helper()
	s := &faStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}},"usage":{"evren":{"credits":2.5}}}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func faPointSO(t *testing.T, s *faStub) {
	t.Helper()
	old := soEndpoint
	soEndpoint = s.srv.URL
	t.Cleanup(func() { soEndpoint = old })
}

// fa1314Frame is a real in-scope tools/call frame. caps=false is the capability-
// missing path used to observe the enforcing -32001 alarm.
func fa1314Frame(id int, state string, caps bool) []byte {
	params := map[string]any{
		"name":      "sim_read",
		"arguments": map[string]any{"path": "sim.txt"},
		"state":     state,
	}
	if caps {
		params["need_label"] = true
		params["no_scratch"] = true
		params["safe"] = true
		params["reversible"] = true
		params["opt_in"] = true
		params["authority"] = true
		params["calibrated"] = true
		params["phase"] = "before-model"
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params,
	})
	return b
}

func faPost(h http.Handler, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/", bytes.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func faReadLines(home string) ([][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("receipt.chain is empty")
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

func faHas(m map[string]any, names ...string) bool {
	for _, n := range names {
		if _, ok := m[n]; ok {
			return true
		}
	}
	return false
}

func faChainVerify(lines [][]byte) (bool, string) {
	if len(lines) < 2 {
		return false, fmt.Sprintf("only %d receipt(s), want at least 2", len(lines))
	}
	prev := []byte{}
	for i, line := range lines {
		var r struct {
			Seq   int    `json:"seq"`
			Prev  string `json:"prev"`
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return false, fmt.Sprintf("line %d not JSON: %v", i, err)
		}
		if r.Seq != i+1 {
			return false, fmt.Sprintf("line %d seq=%d want %d", i, r.Seq, i+1)
		}
		if r.Prev != hex.EncodeToString(prev) {
			return false, fmt.Sprintf("line %d prev=%q want %q", i, r.Prev, hex.EncodeToString(prev))
		}
		if r.Nonce == "" {
			return false, fmt.Sprintf("line %d has no nonce", i)
		}
		h := sha256.New()
		_, _ = h.Write(prev)
		_, _ = h.Write(line)
		prev = h.Sum(nil)
	}
	return true, ""
}

func faTamperDetected(lines [][]byte) bool {
	edited := append([]byte(nil), lines[0]...)
	needle := []byte(`"decision":"`)
	idx := bytes.Index(edited, needle)
	if idx < 0 {
		needle = []byte(`"reason":"`)
		idx = bytes.Index(edited, needle)
	}
	if idx < 0 || idx+len(needle) >= len(edited) {
		return false
	}
	edited[idx+len(needle)] = 'X'
	h := sha256.New()
	_, _ = h.Write(edited)
	var second struct {
		Prev string `json:"prev"`
	}
	if json.Unmarshal(lines[1], &second) != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) != second.Prev
}

func faChildEnv(home, endpoint string) []string {
	env := []string{
		"HOME=" + home,
		"WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT=" + endpoint,
		"EVREN_API_KEY=fa-sim-key",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "WALSY_") ||
			strings.HasPrefix(kv, "EVREN_API_KEY=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// TestSimFA13 simulates the FA13 evidence chain: per-request receipt fields,
// H(n)=sha256(H(n-1)||canonical JSON), per-request emission, and the shipped
// binary writing the same chain. It drives the real advisory HTTP handler and
// a real SO stub; a missing field or link fails the check.
func TestSimFA13(t *testing.T) {
	home := fa1314Env(t, "advisory")
	faCaptureMetrics(t)
	so := faStartSO(t)
	faPointSO(t, so)

	h := mcpHTTPHandler()
	for i, state := range []string{"fa13 sim alpha", "fa13 sim beta"} {
		rec := faPost(h, fa1314Frame(100+i, state, true))
		if rec.Code != http.StatusOK {
			t.Fatalf("advisory HTTP frame %d: status %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if got := so.calls.Load(); got < 2 {
		t.Fatalf("SO stub saw %d POSTs, want at least 2", got)
	}

	lines, err := faReadLines(home)
	if err != nil {
		t.Fatalf("real decisions produced no receipt.chain: %v", err)
	}
	var out []faCheck

	// FA13.1: the full per-request receipt required by the manifest.
	var first map[string]any
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("first receipt not JSON: %v", err)
	}
	type field struct {
		name string
		keys []string
	}
	required := []field{
		{"gateKey", []string{"gate_key", "gateKey"}},
		{"item", []string{"item"}},
		{"phase", []string{"phase"}},
		{"choice", []string{"choice"}},
		{"confBucket", []string{"conf_bucket", "confBucket", "bucket"}},
		{"admitDeny", []string{"admit", "deny", "admit_deny"}},
		{"outcome", []string{"outcome"}},
		{"reason", []string{"reason"}},
		{"latency", []string{"latency_ms", "latency", "elapsed_ms"}},
		{"responseId", []string{"response_id"}},
		{"frameRequestId", []string{"frame_id", "request_id", "frameId", "id"}},
		{"ts", []string{"ts", "timestamp", "time"}},
		{"nonce", []string{"nonce"}},
		{"source", []string{"source"}},
	}
	found, missing := 0, []string{}
	for _, f := range required {
		if faHas(first, f.keys...) {
			found++
		} else {
			missing = append(missing, f.name)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	leaky := bytes.Contains(raw, []byte("fa-sim-key")) || bytes.Contains(raw, []byte("0.95"))
	switch {
	case leaky:
		faAdd(t, &out, "FA13.1", "BROKEN", "receipt leaked the simulated key or raw confidence")
	default:
		faAdd(t, &out, "FA13.1", faGrade(found, len(required)),
			fmt.Sprintf("required receipt fields found=%d/%d missing=[%s] keys=[%s]",
				found, len(required), strings.Join(missing, ","), strings.Join(faMapKeys(first), ",")))
	}
	if first["response_id"] != "none" {
		t.Errorf("FA13.1: response_id=%v want honest literal none", first["response_id"])
	}

	// FA13.2: independently recompute the chain and prove tamper detection.
	chainOK, chainWhy := faChainVerify(lines)
	tamper := faTamperDetected(lines)
	switch {
	case chainOK && tamper:
		faAdd(t, &out, "FA13.2", "WORKS", "seq/prev/nonce recompute; edited first line no longer matches next prev")
	default:
		faAdd(t, &out, "FA13.2", "BROKEN", fmt.Sprintf("chainOK=%v (%s) tamperDetected=%v", chainOK, chainWhy, tamper))
	}

	// FA13.3: one receipt per in-scope request, distinct random nonces.
	n1, n2 := first["nonce"], map[string]any{}
	_ = json.Unmarshal(lines[len(lines)-1], &n2)
	if len(lines) == 2 && n1 != nil && n1 != "" && n1 != n2["nonce"] {
		faAdd(t, &out, "FA13.3", "WORKS", "two in-scope frames produced two receipts with distinct nonces")
	} else {
		faAdd(t, &out, "FA13.3", "PARTIAL", fmt.Sprintf("receipts=%d nonce1=%v nonce2=%v", len(lines), n1, n2["nonce"]))
	}

	// FA13.4: the shipped binary, not just in-process code, writes a chain.
	childHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "./walsy-system1", "-transport", "tools")
	cmd.Env = faChildEnv(childHome, so.srv.URL)
	// Two real in-scope frames: one receipt cannot exercise a hash link.
	twoFrames := append(fa1314Frame(200, "fa13 binary frame one", true), fa1314Frame(201, "fa13 binary frame two", true)...)
	cmd.Stdin = bytes.NewReader(twoFrames)
	var binOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &binOut, &binOut
	err = cmd.Run()
	if ctx.Err() != nil {
		faAdd(t, &out, "FA13.4", "BROKEN", "shipped binary did not exit within 10s")
	} else if err != nil {
		faAdd(t, &out, "FA13.4", "BROKEN", fmt.Sprintf("shipped binary exit error: %v", err))
	} else {
		blines, berr := faReadLines(childHome)
		bchain, bwhy := faChainVerify(blines)
		switch {
		case berr != nil:
			faAdd(t, &out, "FA13.4", "BROKEN", fmt.Sprintf("binary wrote no receipt.chain: %v", berr))
		case len(blines) < 1 || !bchain:
			faAdd(t, &out, "FA13.4", "PARTIAL", fmt.Sprintf("binary receipts=%d chain=%v (%s)", len(blines), bchain, bwhy))
		default:
			faAdd(t, &out, "FA13.4", "WORKS", "shipped binary wrote a verifiable receipt chain from a real in-scope frame")
		}
	}

	t.Logf("FA13 OVERALL: %s", faOverall(out))
}

func faMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// faRecordsHave scans newline-delimited structured records (metrics, audit) for
// any required key. Aliases are accepted so a rename still counts as present.
func faRecordsHave(text string, names ...string) bool {
	for _, line := range strings.Split(text, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &m) != nil {
			continue
		}
		for k := range m {
			lk := strings.ToLower(k)
			for _, want := range names {
				if lk == strings.ToLower(want) {
					return true
				}
			}
		}
	}
	return false
}

// TestSimFA14 simulates FA14 observability and cost against real handlers:
// decision response headers, a structured hash/label/latency record,
// latency_ms + credits in audit.jsonl, the enforcing -32001 alarm, and the
// read-only dashboard header after the feedback surface was removed.
func TestSimFA14(t *testing.T) {
	home := fa1314Env(t, "advisory")
	metrics := faCaptureMetrics(t)
	so := faStartSO(t)
	faPointSO(t, so)

	h := mcpHTTPHandler()
	rec := faPost(h, fa1314Frame(300, "fa14 advisory observation", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("advisory HTTP status=%d body=%s", rec.Code, rec.Body.String())
	}
	if so.calls.Load() == 0 {
		t.Fatal("SO stub saw no POST: real decision path not exercised")
	}

	var out []faCheck

	// FA14.1: decision outcome headers on the real HTTP response.
	wantHeaders := []string{
		"X-Decision-Backend", "X-Decision-Cache",
		"X-Decision-Confidence", "X-Decision-Elapsed-MS",
	}
	present := []string{}
	for _, name := range wantHeaders {
		if rec.Header().Get(name) != "" {
			present = append(present, name)
		}
	}
	faAdd(t, &out, "FA14.1", faGrade(len(present), len(wantHeaders)),
		fmt.Sprintf("decision headers present=[%s] missing=[%s] actual=[%s]",
			strings.Join(present, ","), strings.Join(faMissingHeaders(rec, wantHeaders), ","),
			strings.Join(faHeaderKeys(rec), ",")))

	// FA14.2: a structured log line carrying hash + label + latency.
	auditRaw, _ := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	observe := metrics.String() + "\n" + string(auditRaw)
	hashOK := faRecordsHave(observe, "hash", "state_hash", "sha256", "digest")
	labelOK := faRecordsHave(observe, "label")
	latencyOK := faRecordsHave(observe, "latency_ms", "latency", "elapsed_ms")
	detail := fmt.Sprintf("hash=%v label=%v latency=%v (no slog package in production; keys scanned in metric+audit records)", hashOK, labelOK, latencyOK)
	faAdd(t, &out, "FA14.2", faGrade(faBools(hashOK, labelOK, latencyOK), 3), detail)

	// FA14.3: latency_ms and credits from a real SO call reach audit.jsonl.
	var call map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(auditRaw)), "\n") {
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) == nil && row["event"] == "call" && row["method"] == "systemone" {
			call = row
		}
	}
	if call == nil {
		faAdd(t, &out, "FA14.3", "BROKEN", "no systemone call row in audit.jsonl")
	} else {
		ms, msErr := strconv.ParseInt(call["latency_ms"], 10, 64)
		switch {
		case msErr != nil || ms < 0 || call["credits"] != "2.5":
			faAdd(t, &out, "FA14.3", "PARTIAL", fmt.Sprintf("call row latency_ms=%q credits=%q", call["latency_ms"], call["credits"]))
		default:
			faAdd(t, &out, "FA14.3", "WORKS", fmt.Sprintf("audit.jsonl call row has latency_ms=%d and credits=%s", ms, call["credits"]))
		}
	}

	// FA14.4: real enforcing handler emits the JSON-RPC -32001 alarm.
	t.Setenv("WALSY_SO_MODE", "enforcing")
	eh := mcpHTTPHandler()
	erec := faPost(eh, fa1314Frame(301, "fa14 enforcing alarm", false))
	var reply rpcErrorPayload
	if err := json.Unmarshal(erec.Body.Bytes(), &reply); err != nil {
		faAdd(t, &out, "FA14.4", "BROKEN", fmt.Sprintf("enforcing reply not JSON: %v body=%s", err, erec.Body.String()))
	} else if reply.Error.Code != -32001 {
		faAdd(t, &out, "FA14.4", "BROKEN", fmt.Sprintf("enforcing reply code=%d want -32001 body=%s", reply.Error.Code, erec.Body.String()))
	} else {
		faAdd(t, &out, "FA14.4", "WORKS", fmt.Sprintf("enforcing handler returned -32001: %s", reply.Error.Message))
	}

	// FA14.5: feedback UI removed; backend mutation stays rejected; header links served.
	dh := NewDashboardHandler()
	getRec := httptest.NewRecorder()
	dh.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/feedback", nil))
	postRec := httptest.NewRecorder()
	dh.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/feedback", strings.NewReader(`{"rating":1}`)))
	pageRec := httptest.NewRecorder()
	dh.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil))
	lowerPage := strings.ToLower(pageRec.Body.String())
	feedbackGone := !strings.Contains(lowerPage, `id="feedback"`) && !strings.Contains(lowerPage, "feedback (read-only)") && !strings.Contains(lowerPage, "feedback-text")
	headerOK := strings.Contains(lowerPage, `<a class="brand" href="/"`) &&
		strings.Contains(lowerPage, `href="https://www.linkedin.com/in/kerem-urhan-225870436/"`) &&
		strings.Contains(lowerPage, `href="https://github.com/walsykerem/walsy-system1"`) &&
		strings.Contains(lowerPage, `>github star me</a>`)
	switch {
	case getRec.Code == http.StatusOK && postRec.Code >= 400 && feedbackGone && headerOK:
		faAdd(t, &out, "FA14.5", "WORKS", "feedback GET 200, mutation POST rejected, feedback UI removed, header links served")
	case getRec.Code != http.StatusNotFound && feedbackGone && headerOK:
		faAdd(t, &out, "FA14.5", "PARTIAL", fmt.Sprintf("feedback GET=%d POST=%d feedbackGone=%v headerOK=%v", getRec.Code, postRec.Code, feedbackGone, headerOK))
	default:
		faAdd(t, &out, "FA14.5", "BROKEN", fmt.Sprintf("feedback UI/header contract broken: GET=%d POST=%d feedbackGone=%v headerOK=%v", getRec.Code, postRec.Code, feedbackGone, headerOK))
	}

	t.Logf("FA14 OVERALL: %s", faOverall(out))
}

func faBools(v ...bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}

func faMissingHeaders(rec *httptest.ResponseRecorder, names []string) []string {
	missing := []string{}
	for _, name := range names {
		if rec.Header().Get(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func faHeaderKeys(rec *httptest.ResponseRecorder) []string {
	keys := make([]string, 0, len(rec.Header()))
	for k := range rec.Header() {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
