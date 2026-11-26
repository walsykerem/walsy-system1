package app

// Hermetic behavioural simulations for MANIFEST FA3 and FA4.
// No real SO call, no real toolchain, no process restart, no production edit.
// Every test drives real production code (mcpHTTPHandler, bridgeStdio,
// decisionBridge) through httptest stubs and asserts observable transport
// behaviour: HTTP status/body, stdio forwarding, and real decision outcomes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const w26_caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

type w26_resp struct {
	status int
	body   string
}

func w26_env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-key-fa")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	routerReset()
	t.Cleanup(routerReset)
	return home
}

func w26_so(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig; srv.Close() })
	return srv
}

func w26_answer(choice string, conf float64) string {
	b, _ := json.Marshal(map[string]any{
		"model": "clef",
		"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}},
	})
	return string(b)
}

// w26_http_frame is a full JSON-RPC tools/call envelope with a label-shaped
// state so a real handler routes it through the SO decision path.
func w26_http_frame(id int, state string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"text":"sim"},"state":%q,%s}}`,
		id, state, w26_caps))
}

// w26_flat_frame is a flat params object used with decide()/enforcingGate().
func w26_flat_frame(state string) []byte {
	return []byte(fmt.Sprintf(`{"state":%q,"method":"initialize",%s}`, state, w26_caps))
}

func w26_post(srv *httptest.Server, frame []byte) w26_resp {
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(frame))
	if err != nil {
		return w26_resp{status: -1, body: err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return w26_resp{status: resp.StatusCode, body: string(b)}
}

// ---- FA3: advisory shadow default, post-flush, fail-open ------------------

// FA3(a): no config.txt => advisory mode; FA3(b): a real HTTP handler flushes
// the frame to the client while a blocking SO stub has NOT yet answered.
func TestSimFA3AdvisoryDefaultAndPostFlush(t *testing.T) {
	home := w26_env(t)
	if _, err := os.Stat(filepath.Join(home, ".walsy-system1", "config.txt")); !os.IsNotExist(err) {
		t.Fatalf("FA3 setup: expected no config.txt, stat err=%v", err)
	}
	if mode := soMode(); mode != soModeAdvisory {
		t.Fatalf("FA3 BROKEN: no config.txt => soMode()=%q, want advisory", mode)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	unblock := func() {
		select {
		case release <- struct{}{}:
		default:
		}
	}
	var calls atomic.Int64
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // hold the SO answer open on purpose
		_, _ = io.WriteString(w, w26_answer("no", 0.99))
	}))

	srv := httptest.NewServer(mcpHTTPHandler())
	// Cleanups are LIFO: unblock first, then the two server Close calls, so a
	// failed assertion can never leave a stub blocked inside Close.
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)

	frame := w26_http_frame(31, "classify post flush proof")
	gotCh := make(chan w26_resp, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(frame))
		if err != nil {
			gotCh <- w26_resp{status: -1, body: err.Error()}
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			gotCh <- w26_resp{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		// The handler writes+Flushes before advisory SO. Do NOT ReadAll: with
		// chunked transfer the final chunk only lands when the handler returns.
		buf := make([]byte, len(frame))
		n, _ := io.ReadFull(resp.Body, buf)
		gotCh <- w26_resp{status: resp.StatusCode, body: string(buf[:n])}
	}()

	// The decision path really reached the stub (not a short-circuit).
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("FA3 BROKEN: advisory SO decision never reached the stub")
	}

	// Client must already have the flushed frame while SO is still blocked.
	select {
	case got := <-gotCh:
		if got.status != http.StatusOK || !strings.Contains(got.body, `"id":31`) {
			t.Fatalf("FA3 BROKEN: status=%d body=%s", got.status, got.body)
		}
		if strings.Contains(got.body, "-32001") {
			t.Fatalf("FA3 BROKEN: advisory shadow blocked the frame: %s", got.body)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("FA3 BROKEN: client response waited on blocked SO call (not post-flush)")
	}

	unblock()
	if n := calls.Load(); n != 1 {
		t.Fatalf("FA3: SO calls = %d, want 1", n)
	}
}

// FA3(c): every SO verdict shape (500, high-conf no, low-conf yes, admit)
// still yields HTTP 200 echo. The shadow never changes admission.
func TestSimFA3FailOpenVerdicts(t *testing.T) {
	w26_env(t)
	var mode atomic.Int32
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			_, _ = io.WriteString(w, w26_answer("no", 0.99))
		case 3:
			_, _ = io.WriteString(w, w26_answer("yes", 0.50))
		default:
			_, _ = io.WriteString(w, w26_answer("yes", 0.95))
		}
	}))
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()

	for _, tc := range []struct {
		name string
		mode int32
	}{
		{"so-500", 1},
		{"so-high-conf-no", 2},
		{"so-low-conf", 3},
		{"so-admit", 0},
	} {
		mode.Store(tc.mode)
		got := w26_post(srv, w26_http_frame(40+int(tc.mode), "classify fail-open "+tc.name))
		if got.status != http.StatusOK || !strings.Contains(got.body, `"id":`) {
			t.Fatalf("FA3 BROKEN (%s): status=%d body=%s", tc.name, got.status, got.body)
		}
		if strings.Contains(got.body, "-32001") {
			t.Fatalf("FA3 BROKEN (%s): shadow changed admission: %s", tc.name, got.body)
		}
	}
}

// ---- FA4: enforcement transition, single gate -----------------------------

// Control for FA4: in advisory the same bare frame is forwarded unchanged.
func TestSimFA4AdvisoryForwardsBareFrame(t *testing.T) {
	w26_env(t)
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, w26_answer("yes", 0.95))
	}))
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()

	bare := []byte(`{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"echo","arguments":{"text":"sim"}}}`)
	got := w26_post(srv, bare)
	if got.status != http.StatusOK || !strings.Contains(got.body, `"id":77`) {
		t.Fatalf("FA4 control BROKEN: advisory status=%d body=%s", got.status, got.body)
	}
	if strings.Contains(got.body, "-32001") {
		t.Fatalf("FA4 control BROKEN: advisory blocked a frame: %s", got.body)
	}
}

// FA4: with no config and enforcing mode, the HTTP handler fails closed on a
// frame with no explicit decision capability, using a JSON-RPC error reply.
func TestSimFA4EnforcingBlocksBareFrame(t *testing.T) {
	w26_env(t)
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, w26_answer("yes", 0.95))
	}))
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()

	bare := []byte(`{"jsonrpc":"2.0","id":78,"method":"tools/call","params":{"name":"echo","arguments":{"text":"sim"}}}`)
	got := w26_post(srv, bare)
	if got.status != http.StatusOK {
		t.Fatalf("FA4 BROKEN: enforcing must answer 200 + RPC error, status=%d body=%s", got.status, got.body)
	}
	if !strings.Contains(got.body, "-32001") || !strings.Contains(got.body, `"id":78`) {
		t.Fatalf("FA4 BROKEN: enforcing did not return the block reply: %s", got.body)
	}
	if strings.Contains(got.body, `"text":"sim"`) {
		t.Fatalf("FA4 BROKEN: blocked frame was still echoed: %s", got.body)
	}
}

// FA4: the same single gate is used by the stdio transport (the binary's real
// bridgeStdio). A bare frame is refused to the client and never forwarded.
func TestSimFA4EnforcingSingleGateStdio(t *testing.T) {
	w26_env(t)
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, w26_answer("yes", 0.95))
	}))

	frame := []byte(`{"jsonrpc":"2.0","id":79,"method":"tools/call","params":{"name":"echo","arguments":{"text":"sim"}}}` + "\n")
	var toClient, toServer bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &toClient, &toServer, strings.NewReader(""))

	if !strings.Contains(toClient.String(), "-32001") || !strings.Contains(toClient.String(), `"id":79`) {
		t.Fatalf("FA4 BROKEN: stdio gate reply missing: %q", toClient.String())
	}
	if toServer.Len() != 0 {
		t.Fatalf("FA4 BROKEN: stdio forwarded a blocked frame: %q", toServer.String())
	}
}

// FA4: SO go >= 0.8 admits at the boundary and abstains just below it; and an
// endpoint/key drift after session start fails closed in enforcing mode.
func TestSimFA4SOThresholdAndOracleDrift(t *testing.T) {
	w26_env(t)
	var conf atomic.Value
	conf.Store(0.95)
	w26_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := conf.Load().(float64)
		_, _ = io.WriteString(w, w26_answer("yes", c))
	}))
	d := newDecisionBridge()

	conf.Store(0.80)
	if r := d.decide(w26_flat_frame("classify go boundary exact")); r.Decision != decPin {
		t.Fatalf("FA4 BROKEN: conf 0.80 must admit (go>=0.8): %+v", r)
	}

	conf.Store(0.79)
	if r := d.decide(w26_flat_frame("classify go boundary below")); r.Decision != decPass || r.Reason != "so-low-conf" || !r.Abstained {
		t.Fatalf("FA4 BROKEN: conf 0.79 must abstain: %+v", r)
	}

	// Drift the key identity after the bridge pinned it at construction.
	t.Setenv("EVREN_API_KEY", "sim-key-drifted")
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	init := []byte(`{"jsonrpc":"2.0","id":9,"method":"initialize","params":{` + w26_caps + `}}`)
	resp, blocked := d.enforcingGate(init)
	if !blocked || !strings.Contains(string(resp), "so-unpinned-oracle") {
		t.Fatalf("FA4 BROKEN: oracle drift not fail-closed: blocked=%v resp=%s", blocked, resp)
	}
}
