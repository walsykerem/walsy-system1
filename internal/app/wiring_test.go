package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWiringBridgeDecidesOnInitialize proves the shipped relay calls the real
// decision engine on initialize and adds the MCP marker on the result. SO is a
// local httptest server; no live call is made.
func TestWiringBridgeDecidesOnInitialize(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	}))
	defer srv.Close()

	orig, origTO := soEndpoint, soTimeout
	soEndpoint, soTimeout = srv.URL, 200*time.Millisecond
	defer func() { soEndpoint, soTimeout = orig, origTO }()
	t.Setenv("EVREN_API_KEY", "test-key")

	initReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"},"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"opt_in":true,"phase":"before-model","authority":true,"calibrated":true,"state":"classify this request"}}` + "\n")
	initRes := []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"x"}}}` + "\n")

	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(initReq), &toClient, &toServer, bytes.NewReader(initRes))

	select {
	case <-called:
	default:
		t.Fatal("SO was not called on initialize")
	}
	if !bytes.Equal(toServer.Bytes(), initReq) {
		t.Fatalf("client->server changed: %q", toServer.Bytes())
	}
	if !bytes.Contains(toClient.Bytes(), []byte(markerKey)) {
		t.Fatalf("MCP marker missing on forwarded result: %q", toClient.Bytes())
	}
	// Non-secret identity bytes preserved.
	for _, s := range []string{`"id":1`, `"protocolVersion":"2025-06-18"`, `"name":"x"`} {
		if !bytes.Contains(toClient.Bytes(), []byte(s)) {
			t.Fatalf("lost %q in %q", s, toClient.Bytes())
		}
	}
}

// TestWiringSOFailurePassthrough proves an SO outage never changes the stream.
func TestWiringSOFailurePassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	defer func() { soEndpoint = orig }()
	t.Setenv("EVREN_API_KEY", "test-key")

	req := []byte(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}` + "\n")
	res := []byte(`{"jsonrpc":"2.0","id":2,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(req), &toClient, &toServer, bytes.NewReader(res))
	if !bytes.Equal(toServer.Bytes(), req) || !bytes.Contains(toClient.Bytes(), []byte(`"tools":{}`)) {
		t.Fatalf("passthrough failed: toServer=%q toClient=%q", toServer.Bytes(), toClient.Bytes())
	}
}

// TestHTTPBodyCap proves oversized JSON-RPC bodies get HTTP 413, not OOM.
func TestHTTPBodyCap(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bytes.Repeat([]byte("x"), maxHTTPFrame+1)))
	mcpHTTPHandler().ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestHTTPNoSecretLogging verifies the handler emits no payload/token/conf.
func TestHTTPNoSecretLogging(t *testing.T) {
	var buf bytes.Buffer
	orig := metricOut
	metricOut = &buf
	defer func() { metricOut = orig }()
	t.Setenv("EVREN_API_KEY", "test-key")
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"token":"SECRET-TOKEN","conf":0.95,"cost":918273645,"email":"a@b.com","protocolVersion":"2025-06-18","capabilities":{}}}` + "\n")
	rec := httptest.NewRecorder()
	mcpHTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame)))
	if bytes.Contains(buf.Bytes(), []byte("SECRET-TOKEN")) || bytes.Contains(buf.Bytes(), []byte("a@b.com")) || bytes.Contains(buf.Bytes(), []byte("918273645")) || bytes.Contains(buf.Bytes(), []byte("0.95")) {
		t.Fatalf("secret leaked in metrics: %q", buf.String())
	}
}

// TestUnixSocketModeAndSymlink proves created sockets are 0600 and symlinks refused.
func TestUnixSocketModeAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	l.Close()

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(link); err == nil {
		t.Fatal("symlink socket path was accepted")
	}
}

// TestUnixLiveSocketNotRemoved proves a live listener is never deleted.
func TestUnixLiveSocketNotRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.sock")
	l, err := listenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := listenUnix(path); err == nil {
		t.Fatal("live socket was replaced")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

// TestUpstreamExitMetric proves a nonzero upstream exit is recorded as error.
func TestUpstreamExitMetric(t *testing.T) {
	var buf bytes.Buffer
	orig := metricOut
	metricOut = &buf
	defer func() { metricOut = orig }()

	cmd := exec.Command("sh", "-c", "exit 1")
	if err := cmd.Run(); err == nil {
		t.Fatal("upstream exit 0, want nonzero")
	}
	// Record cmd.Wait() error exactly as runStdio does.
	emitMetric(metric{Seq: 2, PID: os.Getpid(), UpstreamExit: "error", Decision: "passthrough"})

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["upstream_exit"] != "error" {
		t.Fatalf("upstream_exit missing: %v", rec)
	}
	for _, secret := range []string{"secret", "token", "conf", "cost"} {
		if strings.Contains(strings.ToLower(buf.String()), secret) {
			t.Fatalf("%s leaked in metric: %q", secret, buf.String())
		}
	}
}

func enforcingSO(t *testing.T, body string, calls *int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()
	t.Cleanup(routerReset)
}

// The generic gate's "no" means "pass through unchanged" (gate.go's label
// set), so enforcing mode must forward it: blocking there would deny ordinary
// traffic the typed layer never claimed.
func TestEnforcingGenericNoPassesThrough(t *testing.T) {
	enforcingSO(t, `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.96}}}`, nil)
	req := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tool-a","arguments":{},` + explicitCaps + `}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(req), &toClient, &toServer, bytes.NewReader(nil))
	if toServer.Len() == 0 {
		t.Fatalf("generic no was blocked: %q", toClient.Bytes())
	}
	if bytes.Contains(toClient.Bytes(), []byte(`"error"`)) {
		t.Fatalf("generic no produced an error: %q", toClient.Bytes())
	}
}

// A registry item outside its proceed list still blocks in enforcing mode,
// with a valid JSON-RPC error - never an HTTP status or a leaked payload.
func TestEnforcingRegistryRefusalBlocks(t *testing.T) {
	enforcingSO(t, `{"answers":{"gate":{"type":"choice","choice":"destructive","confidence":0.97}}}`, nil)
	req := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"tool-a","arguments":{},` +
		`"state":"the delete already ran on prod","item":"intake.destructive","phase":"before-model",` +
		`"authority":true,"opt_in":true,"calibrated":true,"need_label":true,"no_scratch":true,` +
		`"safe":true,"reversible":true}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(req), &toClient, &toServer, bytes.NewReader(nil))
	if toServer.Len() != 0 {
		t.Fatalf("blocked frame reached upstream: %q", toServer.Bytes())
	}
	if !bytes.Contains(toClient.Bytes(), []byte(`"id":7`)) || !bytes.Contains(toClient.Bytes(), []byte(`"error"`)) {
		t.Fatalf("no JSON-RPC error response: %q", toClient.Bytes())
	}
	if bytes.Contains(toClient.Bytes(), []byte("503")) {
		t.Fatalf("503 leaked: %q", toClient.Bytes())
	}
}

// An SO outage in enforcing mode fails open: frame still reaches upstream.
func TestEnforcingOutageFailsOpen(t *testing.T) {
	enforcingSO(t, `not-json`, nil)
	req := []byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"tool-b",` + explicitCaps + `}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(req), &toClient, &toServer, bytes.NewReader(nil))
	if !bytes.Equal(toServer.Bytes(), req) {
		t.Fatalf("frame not forwarded: %q", toServer.Bytes())
	}
}

// Distinct frames get distinct decisions: the one-shot latch is gone.
func TestPerRequestGating(t *testing.T) {
	var calls int32
	enforcingSO(t, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`, &calls)
	reqs := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tool-c","arguments":{"n":1},` + explicitCaps + `}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tool-c","arguments":{"n":2},` + explicitCaps + `}}` + "\n")
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(reqs), &toClient, &toServer, bytes.NewReader(nil))
	if !bytes.Equal(toServer.Bytes(), reqs) {
		t.Fatalf("frames not forwarded: %q", toServer.Bytes())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("SO calls = %d, want 2 (per-request gating)", got)
	}
}
