package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const n18n19Caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

// simSOFixture wires the real bridge to a local SO stub. No live network, no
// credential reads: the key is an explicit test value.
func simSOFixture(t *testing.T, body string, wire *string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wire != nil {
			b, _ := io.ReadAll(r.Body)
			*wire = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oldEP, oldClient, oldTO := soEndpoint, soClient, soTimeout
	soEndpoint, soClient, soTimeout = srv.URL, srv.Client(), 2*time.Second
	t.Cleanup(func() { soEndpoint, soClient, soTimeout = oldEP, oldClient, oldTO })
	routerReset()
	t.Cleanup(routerReset)
}

func simMetricCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := metricOut
	metricOut = &buf
	t.Cleanup(func() { metricOut = old })
	return &buf
}

func simAuditPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName)
}

func simHealth(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "127.0.0.1:8080"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("health json: %v: %s", err, rec.Body.String())
	}
	return out
}

func TestSimN18HealthFingerprintHTTP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	h := NewDashboardHandler()

	base := simHealth(t, h)
	if base["ok"] != true {
		t.Fatalf("health ok=%v: %v", base["ok"], base)
	}
	sum := sha256.Sum256(decisionItemsJSON)
	if got, want := base["items_sha256"], hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("items_sha256=%v want %s", got, want)
	}
	receipt0, ok := base["receipt_sha256"].(string)
	if !ok || len(receipt0) != 64 {
		t.Fatalf("receipt_sha256=%v want 64-hex string", base["receipt_sha256"])
	}
	if _, ok := base["build_sha"].(string); !ok {
		t.Fatalf("build_sha missing: %v", base)
	}
	if _, ok := base["vcs_modified"].(bool); !ok {
		t.Fatalf("vcs_modified missing: %v", base)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/prompts", strings.NewReader(`{"name":"sim-n18","text":"bind this prompt"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompt write status %d: %s", rec.Code, rec.Body.String())
	}
	afterPrompt := simHealth(t, h)
	if afterPrompt["items_sha256"] != base["items_sha256"] {
		t.Fatalf("items_sha256 changed on prompt write: %v -> %v", base["items_sha256"], afterPrompt["items_sha256"])
	}
	if afterPrompt["receipt_sha256"] == receipt0 {
		t.Fatal("receipt_sha256 did not bind prompt state")
	}

	oldBuild := healthBuildSHA
	healthBuildSHA = "sim-build-rev"
	t.Cleanup(func() { healthBuildSHA = oldBuild })
	afterBuild := simHealth(t, h)
	if afterBuild["receipt_sha256"] == afterPrompt["receipt_sha256"] {
		t.Fatal("receipt_sha256 did not bind build SHA")
	}
}

func TestSimN18DecisionItemsSignature(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	health := simHealth(t, NewDashboardHandler())
	for _, k := range []string{"items_signature", "items_sig", "signature", "sig"} {
		if v, ok := health[k]; ok && v != nil && v != "" {
			return
		}
	}
	for _, name := range []string{"decision_items.json.sig", "decision_items.sig", "decision_items.sha256.sig"} {
		if b, err := os.ReadFile(name); err == nil && len(b) > 0 {
			return
		}
	}
	t.Fatalf("decision_items.json has no verifiable signature surface; health=%v", health)
}

func TestSimN18FrameEquivalenceTransport(t *testing.T) {
	simSOFixture(t, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`, nil)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sim-tool","arguments":{},` + n18n19Caps + `}}` + "\n")

	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &toClient, &toServer, bytes.NewReader(nil))
	if !bytes.Equal(toServer.Bytes(), frame) {
		t.Fatalf("stdio frame changed: got %q want %q", toServer.Bytes(), frame)
	}

	var upstreamBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer up.Close()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mcpHTTPHandlerUpstream(up.URL).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("http status %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamBody != string(frame) {
		t.Fatalf("http frame changed: got %q want %q", upstreamBody, frame)
	}
	if !bytes.Equal(toServer.Bytes(), []byte(upstreamBody)) {
		t.Fatal("stdio and http transports forwarded different frames")
	}
}

func TestSimN19AboveThresholdHelper(t *testing.T) {
	if !aboveThreshold(0.8, 0.8, false) || aboveThreshold(0.8, 0.8, true) ||
		aboveThreshold(0.799999, 0.8, false) || !aboveThreshold(0.800001, 0.8, true) {
		t.Fatal("aboveThreshold boundary semantics wrong")
	}

	item, ok := decisionItemFor("intake.intent")
	if !ok || !item.Gate || len(item.Proceed) == 0 {
		t.Fatalf("intake.intent not a gated item with proceed labels: %+v", item)
	}
	thr := item.threshold(defaultThreshold())
	choice := item.Proceed[0]
	body := fmt.Sprintf(`{"answers":{"gate":{"type":"choice","choice":%q,"confidence":%v}}}`, choice, thr)
	simSOFixture(t, body, nil)

	loose := toolsDecide("intake.intent", "simulate boundary decision", "before-model", false)
	strict := toolsDecide("intake.intent", "simulate boundary decision", "before-model", true)
	var a, b decideReply
	if err := json.Unmarshal([]byte(loose), &a); err != nil {
		t.Fatalf("loose reply: %v: %s", err, loose)
	}
	if err := json.Unmarshal([]byte(strict), &b); err != nil {
		t.Fatalf("strict reply: %v: %s", err, strict)
	}
	if !a.Admitted {
		t.Fatalf("advisory must admit at confidence==threshold: %s", loose)
	}
	if b.Admitted {
		t.Fatalf("strict must deny at confidence==threshold: %s", strict)
	}
}

func TestSimN19ConfBucketMetric(t *testing.T) {
	cases := []struct {
		conf float64
		want string
	}{
		{0.799999, ""}, {0.8, "0.8"}, {0.85, "0.8"},
		{0.9, "0.9"}, {0.95, "0.9"}, {1, "1.0"}, {-0.5, ""},
	}
	for _, tc := range cases {
		if got := confBucket(tc.conf); got != tc.want {
			t.Fatalf("confBucket(%v)=%q want %q", tc.conf, got, tc.want)
		}
	}
	buf := simMetricCapture(t)
	emitMetric(metric{Seq: 1, PID: 2, Decision: decPin, ConfBucket: confBucket(0.95), Covered: true})
	line := buf.String()
	if !strings.Contains(line, `"conf_bucket":"0.9"`) {
		t.Fatalf("metric missing conf_bucket: %s", line)
	}
	for _, bad := range []string{`"confidence"`, `"conf"`, "0.95"} {
		if strings.Contains(line, bad) {
			t.Fatalf("metric leaked %q: %s", bad, line)
		}
	}
}

func TestSimN19AdmitRateWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	routerReset()
	t.Cleanup(routerReset)
	w := &admitWindow{}
	for i := 0; i < 100; i++ {
		w.record(i%4 != 0)
	}
	b, err := os.ReadFile(simAuditPath(t))
	if err != nil {
		t.Fatalf("audit read: %v", err)
	}
	line := string(b)
	for _, want := range []string{`"event":"admit_rate"`, `"count":"75"`, `"status":"window100"`, `"reason":"25"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit missing %s: %s", want, line)
		}
	}
	if strings.Contains(strings.ToLower(line), "confidence") {
		t.Fatalf("audit leaked confidence: %s", line)
	}
}

func TestSimN19AdmitRateDriftAlarm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	routerReset()
	t.Cleanup(routerReset)
	w := &admitWindow{}
	for i := 0; i < 100; i++ {
		w.record(false)
	}
	b, err := os.ReadFile(simAuditPath(t))
	if err != nil {
		t.Fatalf("audit read: %v", err)
	}
	low := strings.ToLower(string(b))
	if !strings.Contains(low, "drift") && !strings.Contains(low, "alarm") {
		t.Fatalf("no admit-rate drift alarm after 0/100 admits: %s", b)
	}
}

func TestSimN19DroppedKeysOnSOWire(t *testing.T) {
	var wire string
	simSOFixture(t, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`, &wire)
	const marker = "SIM-N19-RAW-CONF-MARKER"
	frame := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sim-tool","arguments":{"conf":0.99,"confidence":0.99,"api_key":"` + marker + `"},` + n18n19Caps + `}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &toClient, &toServer, bytes.NewReader(nil))
	if wire == "" {
		t.Fatal("SO was not called; capability setup failed")
	}
	for _, bad := range []string{`"conf"`, `"confidence"`, marker, "0.99"} {
		if strings.Contains(strings.ToLower(wire), strings.ToLower(bad)) {
			t.Fatalf("SO wire leaked %q: %s", bad, wire)
		}
	}
	b, err := os.ReadFile(simAuditPath(t))
	if err != nil {
		t.Fatalf("audit read: %v", err)
	}
	for _, bad := range []string{`"confidence"`, marker, "0.99"} {
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(bad)) {
			t.Fatalf("audit leaked %q: %s", bad, b)
		}
	}
}

func TestSimN19RawConfidenceNotAudited(t *testing.T) {
	buf := simMetricCapture(t)
	var wire string
	simSOFixture(t, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`, &wire)
	frame := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sim-tool","arguments":{},` + n18n19Caps + `}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &toClient, &toServer, bytes.NewReader(nil))
	if wire == "" {
		t.Fatal("SO was not called; capability setup failed")
	}
	b, err := os.ReadFile(simAuditPath(t))
	if err != nil {
		t.Fatalf("audit read: %v", err)
	}
	sinks := []struct {
		name string
		data string
	}{
		{"audit", string(b)}, {"metric", buf.String()}, {"wire", wire},
	}
	for _, s := range sinks {
		for _, bad := range []string{`"confidence"`, `"conf"`, "0.95"} {
			if strings.Contains(strings.ToLower(s.data), bad) {
				t.Fatalf("%s leaked %q: %s", s.name, bad, s.data)
			}
		}
	}
}
