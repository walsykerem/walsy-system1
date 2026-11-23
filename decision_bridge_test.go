package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// frameGateWriter detects two clientOut writes in flight at once: the upstream
// frame (main goroutine) releases the blocked client frame and waits a bounded
// window for the client-reader goroutine to enter Write. With the bridge's
// write mutex the client goroutine blocks instead and the window expires.
type frameGateWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	release chan struct{}
	client  chan struct{}
	overlap bool
}

func (w *frameGateWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"blocked:`)) {
		close(w.client)
	} else {
		close(w.release)
		select {
		case <-w.client:
			w.overlap = true
		case <-time.After(2 * time.Second):
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// explicitCaps is the full opt-in set enforcing gate requires; wire tests use
// it so unrelated guards stay the reason a frame is blocked.
const explicitCaps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func gateFrame(method string, id int) []byte {
	if method == "tools/call" {
		return []byte(`{"jsonrpc":"2.0","id":` + itoaTest(id) +
			`,"method":"tools/call","params":{"name":"read_file",` + explicitCaps + `}}`)
	}
	return []byte(`{"jsonrpc":"2.0","id":` + itoaTest(id) +
		`,"method":"` + method + `","params":{` + explicitCaps + `}}`)
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type gateReply struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestBudgetGateTable(t *testing.T) {
	cases := []struct {
		name    string
		budget  string
		frames  []string // method per frame, in order
		blocked []bool   // expected blocked flag per frame
	}{
		{
			name:    "missing budget passes",
			budget:  "",
			frames:  []string{"tools/call"},
			blocked: []bool{false},
		},
		{
			name:    "malformed budget passes",
			budget:  "lots",
			frames:  []string{"tools/call"},
			blocked: []bool{false},
		},
		{
			name:    "exhausted budget blocks and steers",
			budget:  "1",
			frames:  []string{"tools/call", "tools/call", "tools/call"},
			blocked: []bool{false, true, true},
		},
		{
			name:    "initialize refills and admits again",
			budget:  "1",
			frames:  []string{"tools/call", "tools/call", "initialize", "tools/call"},
			blocked: []bool{false, true, false, false},
		},
		{
			name:    "explicit zero budget blocks",
			budget:  "0",
			frames:  []string{"tools/call"},
			blocked: []bool{true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRouterForTest(t)
			t.Setenv("EVREN_API_KEY", "")
			t.Setenv("WALSY_SO_MODE", "")
			t.Setenv("WALSY_ROUTER_RETRY_BUDGET", tc.budget)
			routerReset()
			d := newDecisionBridge()
			for i, method := range tc.frames {
				resp, blocked := d.enforcingGate(gateFrame(method, i+1))
				if blocked != tc.blocked[i] {
					t.Fatalf("frame %d (%s) blocked=%v want %v resp=%s",
						i, method, blocked, tc.blocked[i], resp)
				}
				if !blocked {
					continue
				}
				var reply gateReply
				if err := json.Unmarshal(resp, &reply); err != nil {
					t.Fatalf("frame %d refusal not JSON: %v %q", i, err, resp)
				}
				if reply.Error.Code != -32001 {
					t.Fatalf("frame %d code = %d, want -32001", i, reply.Error.Code)
				}
				for _, want := range []string{"initialize", "RETRY_BUDGET", "alternative"} {
					if !strings.Contains(reply.Error.Message, want) {
						t.Fatalf("frame %d steer missing %q: %q", i, want, reply.Error.Message)
					}
				}
			}
		})
	}
}

func TestRouterDenyEnforcingBlocksAdvisoryAudits(t *testing.T) {
	// The deny list is deterministic policy: enforcing refuses it outright,
	// advisory records it via routerRecord but still forwards.
	for _, tc := range []struct {
		mode    string
		blocked bool
	}{
		{"enforcing", true},
		{"advisory", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			resetRouterForTest(t)
			t.Setenv("EVREN_API_KEY", "")
			t.Setenv("WALSY_ROUTER_DENY", "read_file")
			t.Setenv("WALSY_SO_MODE", tc.mode)
			routerReset()
			d := newDecisionBridge()
			resp, blocked := d.enforcingGate(gateFrame("tools/call", 1))
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v want %v resp=%s", blocked, tc.blocked, resp)
			}
			if !blocked {
				return
			}
			var reply gateReply
			if err := json.Unmarshal(resp, &reply); err != nil {
				t.Fatalf("refusal not JSON: %v %q", err, resp)
			}
			if reply.Error.Code != -32001 || !strings.Contains(reply.Error.Message, "denied by router policy") {
				t.Fatalf("wrong refusal: code=%d msg=%q", reply.Error.Code, reply.Error.Message)
			}
		})
	}
}

// A4: a degraded/transient outcome must not be cached. Two identical calls
// must both reach SO rather than serve a stale abstain forever.
func TestTransientNotCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	defer func() { soEndpoint = orig }()
	t.Setenv("EVREN_API_KEY", "test-key")

	d := newDecisionBridge()
	req := []byte(`{"state":"classify this request","method":"initialize",` + explicitCaps + `}`)
	r1 := d.decide(req)
	r2 := d.decide(req)
	if r1.Decision != decPass || r2.Decision != decPass {
		t.Fatalf("degraded outcomes must fail open: %+v / %+v", r1, r2)
	}
	if calls != 2 {
		t.Fatalf("degraded result was cached: SO called %d times, want 2", calls)
	}
	if _, hit := d.cache.lookup(gateKeyBytes(req, soCapability())); hit {
		t.Fatal("transient/degraded outcome must not be stored in cache")
	}
}

// B1: volatile envelope/identifier fields must not fragment the gate cache,
// while a semantic change must.
func TestGateKeyCanonicalizesVolatileFields(t *testing.T) {
	cap := soCapability()
	base := []byte(`{"name":"read_file","arguments":{"path":"/tmp/a","timestamp":1,"messageRef":"m1","sessionId":"s1"}}`)
	variant := []byte(`{"name":"read_file","arguments":{"path":"/tmp/a","timestamp":999,"messageRef":"m999","sessionId":"s999"},"id":88,"jsonrpc":"2.0"}`)
	if a, b := gateKey(base, cap), gateKey(variant, cap); a != b {
		t.Fatalf("volatile fields changed gateKey: %s != %s", a, b)
	}
	semantic := []byte(`{"name":"write_file","arguments":{"path":"/tmp/a","timestamp":1,"messageRef":"m1","sessionId":"s1"}}`)
	if a, b := gateKey(base, cap), gateKey(semantic, cap); a == b {
		t.Fatalf("semantic change did not change gateKey: %s", a)
	}
}

// A8: the raw-args secret scan must run even when the call carries a file body,
// so metadata.api_key alongside content cannot escape the diff-only path.
func TestArgsSecretCheckedWithBody(t *testing.T) {
	resetRouterForTest(t)
	d := newDecisionBridge()
	// The write must name a path this session already read, so the unread-path
	// write guard passes and the argument-level secret scan is what refuses.
	d.guard.check("read_file", json.RawMessage(`{"path":"/tmp/args-secret.txt"}`))
	// AKIA literal is a shape pattern that argsSecretCheck sees inside raw JSON
	// (a quoted api_key assignment would not match the assignment regex).
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"filePath":"/tmp/args-secret.txt","content":"hello world","metadata":{"api_key":"AKIAIOSFODNN7EXAMPLE"}},` + explicitCaps + `}}`)
	resp, blocked := d.enforcingGate(req)
	if !blocked {
		t.Fatal("write call with content + metadata api_key must be blocked")
	}
	var reply gateReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("refusal not JSON: %v %q", err, resp)
	}
	if reply.Error.Code != -32001 || !strings.Contains(reply.Error.Message, "secret") {
		t.Fatalf("want diff-secret refusal, got code=%d msg=%q", reply.Error.Code, reply.Error.Message)
	}
}

// The enforcing block reply and the forwarded upstream frame must never be in
// clientOut at once: interleaved sub-PIPE_BUF writes would corrupt JSON-RPC
// framing. See frameGateWriter for how overlap is observed.
func TestBridgeStdioSerializesClientWrites(t *testing.T) {
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("HOME", t.TempDir())
	routerReset()
	defer routerReset()

	pr, pw := io.Pipe()
	gate := &frameGateWriter{release: make(chan struct{}), client: make(chan struct{})}
	// The blocked client frame is released only once the upstream frame's write
	// has started, so a client reply entering Write at the same time proves the
	// two writes were concurrent.
	go func() {
		<-gate.release
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_file","arguments":{"path":"/tmp/neverread-xyz.txt","content":"x"},` + explicitCaps + `}}` + "\n"))
		_ = pw.Close()
	}()

	upOut := bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":9,"result":{}}` + "\n"))
	var toServer bytes.Buffer
	bridgeStdio(pr, gate, &toServer, upOut)

	if gate.overlap {
		t.Fatal("two clientOut writes overlapped: the bridge must serialize them")
	}
	if !bytes.Contains(gate.buf.Bytes(), []byte(`"id":5`)) {
		t.Fatalf("blocked reply not delivered: %q", gate.buf.Bytes())
	}
}

// N4: enforcing must fail closed on a frame with no explicit decision
// capability; advisory only warns. No default-true opt-in backfill.
func TestN4MissingCapabilityFailClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	if explicitCapability([]byte(`{"state":"classify this"}`)) {
		t.Fatal("bare frame must not count as explicit consent")
	}
	if !explicitCapability([]byte(`{"state":"classify this",` + explicitCaps + `}`)) {
		t.Fatal("full opt-in set must be accepted")
	}
	d := newDecisionBridge()
	_, blocked := d.enforcingGate([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`))
	if !blocked {
		t.Fatal("enforcing must fail closed on missing capability")
	}
}

// TestConfigDeclaredCapabilityNeverBackfillsMCP proves config.txt cannot mint
// opt-ins into an MCP frame: consent must be explicit on the frame itself.
func TestConfigDeclaredCapabilityNeverBackfillsMCP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	writeSOConfigForTest(t, "so_mode=enforcing\nneed_label=true\nno_scratch=true\nsafe=true\nreversible=true\nauthority=true\ncalibrated=true\nopt_in=true\nphase=before-model\n", 0600)

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`)
	f, err := decodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	params := unwrapParams(raw, f)
	// Consent is never invented: config fields are not frame capabilities.
	if explicitCapability(params) {
		t.Fatalf("config-declared opt-ins must not satisfy capability gate: %s", params)
	}
	d := newDecisionBridge()
	// The frame carries no opt-ins, so enforcing must fail closed.
	if _, blocked := d.enforcingGate(raw); !blocked {
		t.Fatal("config-declared opt-ins must not unblock an opt-in-free frame")
	}
	state := buildSOState(string(params))
	if strings.Contains(state, `"opt_in":true`) || strings.Contains(state, `"phase":"before-model"`) {
		t.Fatalf("SO state must not invent config-declared opt-in: %s", state)
	}
}

// A generation change via syncHold clears the strong hold; the locked helper
// must not report it for the old generation.
func TestHoldStrongForGenerationChange(t *testing.T) {
	d := &decisionBridge{}
	d.setHold(7, 0.9)
	if !d.holdStrongFor(7) {
		t.Fatal("strong hold lost immediately after setHold")
	}
	d.syncHold(8)
	if d.holdStrongFor(7) {
		t.Fatal("stale generation must not inherit strong hold after syncHold")
	}
	if d.holdStrongFor(8) {
		t.Fatal("syncHold cleared holdStrong on generation change")
	}
}

// A Bearer token or key=value secret in a free-text string field must not
// reach the mirror; ordinary text must survive intact.
func TestScrubbedMirrorScrubsFreeTextSecrets(t *testing.T) {
	req := []byte(`{"params":{"note":"Authorization: Bearer s3cr3t-abc","detail":"api_key=leak123 keepme","plain":"hello world"},"secret":"do-not-leak"}`)
	got := string(scrubbedMirror(req))
	if strings.Contains(got, "s3cr3t-abc") || strings.Contains(got, "leak123") {
		t.Fatalf("secret leaked into mirror body: %s", got)
	}
	if strings.Contains(got, "do-not-leak") {
		t.Fatalf("secret-keyed field survived into mirror body: %s", got)
	}
	if !strings.Contains(got, "hello world") || !strings.Contains(got, "keepme") {
		t.Fatalf("ordinary text was lost: %s", got)
	}
}

// FIX 2: a notification (no id) on a gated method must be consumed without a
// reply; the same frame with an id still gets an error reply.
func TestEnforcingGateNotificationNoReply(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	t.Setenv("WALSY_ROUTER_DENY", "read_file")
	routerReset()
	d := newDecisionBridge()
	notification := []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"read_file"}}`)
	if resp, blocked := d.enforcingGate(notification); !blocked || len(resp) != 0 {
		t.Fatalf("notification must be gated with no reply: blocked=%v resp=%q", blocked, resp)
	}
	request := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read_file"}}`)
	resp, blocked := d.enforcingGate(request)
	if !blocked || len(resp) == 0 {
		t.Fatalf("request with id must get an error reply: blocked=%v resp=%q", blocked, resp)
	}
	var reply gateReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("id reply not JSON: %v %q", err, resp)
	}
	if reply.Error.Code != -32001 {
		t.Fatalf("id reply code = %d, want -32001", reply.Error.Code)
	}
}

// FIX 2: an initialize notification still runs routerBudgetReset.
func TestEnforcingGateInitializeNotificationRefills(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "1")
	routerReset()
	d := newDecisionBridge()
	initNotification := []byte(`{"jsonrpc":"2.0","method":"initialize","params":{}}`)
	if resp, blocked := d.enforcingGate(initNotification); !blocked || len(resp) != 0 {
		t.Fatalf("initialize notification must be gated with no reply: blocked=%v resp=%q", blocked, resp)
	}
	// The initialize notification refilled the budget; a capped first call passes.
	call := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file",` + explicitCaps + `}}`)
	if _, blocked := d.enforcingGate(call); blocked {
		t.Fatal("first capped tool call after initialize notification must pass")
	}
}
