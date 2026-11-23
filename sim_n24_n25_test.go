package main

// Static-only simulation proof for MANIFEST items N24 and N25.
// Everything is local: temp HOME, dummy key, httptest SO stub, loopback HTTP.
// No live System One call, no credential read, no repo mutation.

import (
	"bytes"
	"encoding/json"
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

const simSOAnswer = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.93,"criteria":{"yes":"admit","no":"pass","other":"ambiguous"},"probabilities":{"yes":0.93,"no":0.04,"other":0.03}}},"usage":{}}`

type simSOStub struct {
	srv  *httptest.Server
	hits atomic.Int64
	mu   sync.Mutex
}

func newSimSOStub(t *testing.T) *simSOStub {
	t.Helper()
	s := &simSOStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, simSOAnswer)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *simSOStub) count() int { return int(s.hits.Load()) }

// simHermetic pins every config source. WALSY_SO_MODE="" means no config file
// and the advisory default; the key is a dummy string, never a real credential.
func simHermetic(t *testing.T, so *simSOStub) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-n24n25-dummy-key")
	t.Setenv("WALSY_SO_ENDPOINT", so.srv.URL)
	old := soEndpoint
	soEndpoint = so.srv.URL
	t.Cleanup(func() { soEndpoint = old })
	routerReset()
}

func simN24Frame(id int, method string, params map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	return b
}

func simParams(phase string) map[string]any {
	p := map[string]any{
		"name":       "sim.tool",
		"arguments":  map[string]any{},
		"state":      "classify this request",
		"need_label": true,
		"no_scratch": true,
		"safe":       true,
		"reversible": true,
		"authority":  true,
		"calibrated": true,
		"opt_in":     true,
	}
	if phase != "" {
		p["phase"] = phase
	}
	return p
}

// N24: JSON-RPC method, not an MCP before_model hook, must decide scope.
// Observe the real single decision hook and count hits on the SO stub.
func TestSimN24MethodScope(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		phase      string
		wantHits   int
		wantReason string
	}{
		{name: "control_method_and_phase", method: "tools/call", phase: "before_model", wantHits: 1},
		{name: "method_only_no_phase", method: "tools/call", phase: "", wantHits: 0, wantReason: "layer0-scope"},
		{name: "out_of_scope_method", method: "resources/subscribe", phase: "before_model", wantHits: 0, wantReason: "layer0-scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			so := newSimSOStub(t)
			simHermetic(t, so)
			d := newDecisionBridge()
			r := d.decideFrame(simN24Frame(1, tc.method, simParams(tc.phase)))
			if got := so.count(); got != tc.wantHits {
				t.Errorf("SO POSTs = %d, want %d; decision = %+v", got, tc.wantHits, r)
			}
			if tc.wantReason != "" && r.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tc.wantReason)
			}
		})
	}
}

// N24: empty model phase is fail-closed out-of-scope (stricter than the old
// phase-optional expectation shared with FA2/FA5).
func TestSimN24PhaseDependency(t *testing.T) {
	base := stateEnvelope{
		State: "classify this request", Method: "tools/call", Phase: "before_model",
		NeedLabel: true, NoScratch: true, Safe: true, Reversible: true,
		Authority: true, OptIn: true, Calibrated: true,
	}
	if !inScope(base) {
		t.Fatal("control: in-scope method with model phase must be in scope")
	}
	noPhase := base
	noPhase.Phase = ""
	if inScope(noPhase) {
		t.Errorf("N24: inScope accepts tools/call with no model phase; empty phase must be fail-closed out-of-scope")
	}
}

// N24 through the real Streamable HTTP handler (advisory, post-flush SO).
func TestSimN24HTTPTransportScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		wantHits int
	}{
		{name: "in_scope", method: "tools/call", wantHits: 1},
		{name: "out_of_scope", method: "resources/subscribe", wantHits: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			so := newSimSOStub(t)
			simHermetic(t, so)
			h := mcpHTTPHandlerUpstream("")
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(simN24Frame(7, tc.method, simParams("before_model"))))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if got := so.count(); got != tc.wantHits {
				t.Errorf("SO POSTs = %d, want %d", got, tc.wantHits)
			}
		})
	}
}

// N25: Mcp-Method <-> body.method and Mcp-Name <-> body.params.name must match.
func TestSimN25HeaderBodyMatch(t *testing.T) {
	cases := []struct {
		name      string
		hdrMethod string
		hdrName   string
		wantCode  int
	}{
		{name: "matching_method", hdrMethod: "tools/call", wantCode: http.StatusOK},
		{name: "mismatched_method", hdrMethod: "resources/read", wantCode: http.StatusBadRequest},
		{name: "matching_name", hdrName: "sim.tool", wantCode: http.StatusOK},
		{name: "mismatched_name", hdrName: "other.tool", wantCode: http.StatusBadRequest},
		{name: "absent_headers", wantCode: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			so := newSimSOStub(t)
			simHermetic(t, so)
			h := mcpHTTPHandlerUpstream("")
			params := map[string]any{"name": "sim.tool", "arguments": map[string]any{}}
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(simN24Frame(3, "tools/call", params)))
			if tc.hdrMethod != "" {
				req.Header.Set(mcpMethodHeader, tc.hdrMethod)
			}
			if tc.hdrName != "" {
				req.Header.Set(mcpNameHeader, tc.hdrName)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK && tc.hdrMethod != "" && rec.Header().Get(mcpMethodHeader) != tc.hdrMethod {
				t.Errorf("Mcp-Method not mirrored: %q", rec.Header().Get(mcpMethodHeader))
			}
			if tc.wantCode == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "invalid MCP headers") {
				t.Errorf("400 body = %q, want invalid MCP headers", rec.Body.String())
			}
		})
	}
}

// N25: advertising the capability is default-off and never a gate by itself.
func TestSimN25CapabilityMarkerInert(t *testing.T) {
	so := newSimSOStub(t)
	simHermetic(t, so)
	initResult := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"up","version":"1"}}}`)
	out := newDecisionBridge().inspectServer(initResult)
	var env struct {
		Result struct {
			Capabilities struct {
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	raw, ok := env.Result.Capabilities.Experimental[markerKey]
	if !ok {
		t.Fatalf("capability marker %q not advertised: %s", markerKey, out)
	}
	var v struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Enabled {
		t.Fatalf("marker must be default-off (enabled:false), got %s", raw)
	}

	h := mcpHTTPHandlerUpstream("")
	frame := simN24Frame(2, "tools/call", map[string]any{"name": "sim.tool", "arguments": map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("advertised capability alone blocked a frame: %s", rec.Body.String())
	}
}

// simCleanEnv keeps real credentials out of the spawned binary.
func simCleanEnv(extra ...string) []string {
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if k == "HOME" || k == "EVREN_API_KEY" || strings.HasPrefix(k, "EVREN_") ||
			strings.HasPrefix(k, "WALSY_SO_") || strings.HasSuffix(k, "_API_KEY") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// N25 end-to-end: real compiled binary, real loopback listener, real client.
func TestSimN25BinaryHTTPHeaders(t *testing.T) {
	so := newSimSOStub(t)
	bin, err := filepath.Abs("walsy-system1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("binary unavailable: %v", err)
	}
	metricPath := filepath.Join(t.TempDir(), "metric.log")
	lf, err := os.Create(metricPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()

	cmd := exec.Command(bin, "-transport", "http", "-listen", "127.0.0.1:0")
	cmd.Env = simCleanEnv("HOME="+t.TempDir(), "WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT="+so.srv.URL, "EVREN_API_KEY=sim-n24n25-dummy-key")
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	addr := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && addr == "" {
		b, _ := os.ReadFile(metricPath)
		for _, line := range strings.Split(string(b), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			if a, ok := m["listen_addr"].(string); ok && a != "" {
				addr = a
				break
			}
		}
		if addr == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if addr == "" {
		t.Fatalf("binary emitted no listen_addr; metric = %s", metricPath)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	post := func(hdrMethod, hdrName string) (int, string) {
		t.Helper()
		frame := simN24Frame(9, "tools/call", map[string]any{"name": "sim.tool", "arguments": map[string]any{}})
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/", bytes.NewReader(frame))
		if err != nil {
			t.Fatal(err)
		}
		if hdrMethod != "" {
			req.Header.Set(mcpMethodHeader, hdrMethod)
		}
		if hdrName != "" {
			req.Header.Set(mcpNameHeader, hdrName)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, tc := range []struct {
		name      string
		hdrMethod string
		hdrName   string
		want      int
	}{
		{name: "matching_method", hdrMethod: "tools/call", want: http.StatusOK},
		{name: "mismatched_method", hdrMethod: "resources/read", want: http.StatusBadRequest},
		{name: "mismatched_name", hdrName: "other.tool", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := post(tc.hdrMethod, tc.hdrName)
			if code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", code, tc.want, body)
			}
		})
	}
}
