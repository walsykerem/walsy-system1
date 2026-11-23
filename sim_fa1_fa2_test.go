package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// w25_stub is an in-process System One endpoint. No real SO call is made.
type w25_stub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	reply  string
}

const w25_yesReply = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9,"probabilities":{"yes":0.9,"no":0.1}}}}`
const w25_buildReply = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"build","confidence":0.9,"probabilities":{"build":0.9,"chat":0.1}}}}`

func w25_newStub() *w25_stub {
	s := &w25_stub{reply: w25_yesReply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		reply := s.reply
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	return s
}

func (s *w25_stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *w25_stub) body(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.bodies) {
		return ""
	}
	return s.bodies[i]
}

func (s *w25_stub) setReply(v string) {
	s.mu.Lock()
	s.reply = v
	s.mu.Unlock()
}

func w25_env(t *testing.T, mode string) *w25_stub {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", mode)
	t.Setenv("EVREN_API_KEY", "sim-w25-only-key")
	t.Setenv("WALSY_ROUTER_DENY", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "16")
	routerReset()
	t.Cleanup(routerReset)
	stub := w25_newStub()
	orig := soEndpoint
	soEndpoint = stub.srv.URL
	t.Cleanup(func() {
		soEndpoint = orig
		stub.srv.Close()
	})
	return stub
}

func w25_waitCount(t *testing.T, s *w25_stub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SO POST count = %d, want >= %d", s.count(), want)
}

const w25_capsNoPhase = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,` +
	`"authority":true,"calibrated":true,"opt_in":true`

const w25_caps = w25_capsNoPhase + `,"phase":"before-model"`

func w25_frame(method string, id int, params string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s}}`, id, method, params))
}

func w25_do(t *testing.T, method, url, body, origin string) string {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d body %s", method, url, resp.StatusCode, b)
	}
	return string(b)
}

type w25_soRequest struct {
	Model     string `json:"model"`
	State     string `json:"state"`
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

func w25_checkGenericQuestion(t *testing.T, body string) {
	t.Helper()
	var req w25_soRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("SO body not JSON: %v: %s", err, body)
	}
	if req.Model != "clef" {
		t.Fatalf("SO model = %q, want clef", req.Model)
	}
	if len(req.Questions) != 1 {
		t.Fatalf("SO questions = %d, want 1: %s", len(req.Questions), body)
	}
	q, ok := req.Questions["gate"]
	if !ok {
		t.Fatalf("SO question key missing gate: %s", body)
	}
	if q.Type != "choice" {
		t.Fatalf("SO question type = %q, want choice", q.Type)
	}
	for _, label := range []string{"yes", "no", "other"} {
		if _, ok := q.Criteria[label]; !ok {
			t.Fatalf("SO criteria missing %q: %s", label, body)
		}
	}
	if strings.Contains(body, `"id"`) {
		t.Fatalf("SO body carries a client frame id: %s", body)
	}
}

// FA1: one decide hook per in-scope frame, and no SO POST outside it.
func TestSimFA1SingleHookAndScope(t *testing.T) {
	stub := w25_env(t, "advisory")
	d := newDecisionBridge()

	r := d.decideFrame(w25_frame("tools/call", 1, `"state":"decide: sim one",`+w25_caps))
	w25_waitCount(t, stub, 1)
	if r.Decision != decPin || r.Reason != "so-admit" {
		t.Fatalf("in-scope: decision=%q reason=%q, want pin/so-admit", r.Decision, r.Reason)
	}
	if stub.count() != 1 {
		t.Fatalf("in-scope SO POST count = %d, want 1", stub.count())
	}

	out := d.decideFrame(w25_frame("resources/list", 2, `"state":"decide: sim out",`+w25_caps))
	if out.Reason != "layer0-scope" || out.Decision == decPin {
		t.Fatalf("out-of-scope: decision=%q reason=%q, want pass/layer0-scope", out.Decision, out.Reason)
	}
	if stub.count() != 1 {
		t.Fatalf("out-of-scope added SO POST: count = %d, want 1", stub.count())
	}

	second := d.decideFrame(w25_frame("tools/call", 3, `"state":"decide: sim two",`+w25_caps))
	w25_waitCount(t, stub, 2)
	if second.Decision != decPin {
		t.Fatalf("second in-scope frame: decision=%q reason=%q, want pin", second.Decision, second.Reason)
	}
	if stub.count() != 2 {
		t.Fatalf("second in-scope SO POST count = %d, want 2 (one-shot latch?)", stub.count())
	}
	w25_checkGenericQuestion(t, stub.body(0))
}

// w25_closeListener signals when serveUnix closes its accepted connection.
// bridgeStdio returns only after its advisory worker finishes, so waiting on
// done also waits for the receipt writer before HOME's TempDir is removed.
type w25_closeListener struct {
	net.Listener
	done chan struct{}
}

func (l *w25_closeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &w25_closeConn{Conn: c, done: l.done}, nil
}

type w25_closeConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *w25_closeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// FA1: stdio, HTTP, unix and tools transports route through the same gate.
// Dashboard is probed for stray SO POSTs; it has no gate hook.
func TestSimFA1TransportsNoStrayPosts(t *testing.T) {
	t.Run("stdio", func(t *testing.T) {
		stub := w25_env(t, "advisory")
		in1 := append(w25_frame("tools/call", 1, `"state":"decide: stdio one",`+w25_caps), '\n')
		in2 := append(w25_frame("resources/list", 2, `"state":"decide: stdio two",`+w25_caps), '\n')
		var upstream, client bytes.Buffer
		bridgeStdio(bytes.NewReader(append(append([]byte{}, in1...), in2...)), &client, &upstream, strings.NewReader(""))
		if !strings.Contains(upstream.String(), `"method":"tools/call"`) ||
			!strings.Contains(upstream.String(), `"method":"resources/list"`) {
			t.Fatalf("stdio did not forward both frames: %s", upstream.String())
		}
		w25_waitCount(t, stub, 1)
		if stub.count() != 1 {
			t.Fatalf("stdio SO POST count = %d, want 1", stub.count())
		}
	})

	t.Run("http", func(t *testing.T) {
		stub := w25_env(t, "advisory")
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
		}))
		defer up.Close()
		srv := httptest.NewServer(mcpHTTPHandlerUpstream(up.URL))
		defer srv.Close()
		w25_do(t, http.MethodPost, srv.URL,
			string(w25_frame("tools/call", 1, `"state":"decide: http one",`+w25_caps)), "")
		w25_waitCount(t, stub, 1)
		w25_do(t, http.MethodPost, srv.URL,
			string(w25_frame("resources/list", 2, `"state":"decide: http out",`+w25_caps)), "")
		if stub.count() != 1 {
			t.Fatalf("http SO POST count = %d, want 1", stub.count())
		}
	})

	t.Run("unix", func(t *testing.T) {
		stub := w25_env(t, "advisory")
		dir, err := os.MkdirTemp("", "w25sock")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		path := filepath.Join(dir, "s.sock")
		l, err := listenUnix(path)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		done := make(chan struct{})
		go func() {
			serveUnix(&w25_closeListener{Listener: l, done: done}, func() (net.Conn, error) {
				up, peer := net.Pipe()
				go func() {
					br := bufio.NewReader(peer)
					for i := 0; i < 2; i++ {
						_, _ = br.ReadBytes('\n')
					}
					_ = peer.Close()
				}()
				return up, nil
			})
		}()
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		in1 := append(w25_frame("tools/call", 1, `"state":"decide: unix one",`+w25_caps), '\n')
		in2 := append(w25_frame("resources/list", 2, `"state":"decide: unix out",`+w25_caps), '\n')
		if _, err := conn.Write(append(append([]byte{}, in1...), in2...)); err != nil {
			t.Fatal(err)
		}
		w25_waitCount(t, stub, 1)
		if stub.count() != 1 {
			t.Fatalf("unix SO POST count = %d, want 1", stub.count())
		}
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("serveUnix did not close accepted unix connection")
		}
	})

	t.Run("tools", func(t *testing.T) {
		stub := w25_env(t, "advisory")
		var out bytes.Buffer
		runToolsServer(bytes.NewReader(append(w25_frame("tools/list", 1, ""), '\n')), &out)
		if stub.count() != 0 {
			t.Fatalf("tools/list SO POST count = %d, want 0", stub.count())
		}
		out.Reset()
		stub.setReply(w25_buildReply)
		tc := w25_frame("tools/call", 2,
			`"name":"system1_decide","arguments":{"item":"intake.intent","state":"classify this request","phase":"before-model"}`)
		runToolsServer(bytes.NewReader(append(tc, '\n')), &out)
		w25_waitCount(t, stub, 1)
		if stub.count() != 1 {
			t.Fatalf("tools/call system1_decide SO POST count = %d, want 1", stub.count())
		}
		var rpc struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &rpc); err != nil {
			t.Fatalf("tools reply not JSON: %v: %s", err, out.String())
		}
		text := ""
		for _, c := range rpc.Result.Content {
			text += c.Text
		}
		if !strings.Contains(text, `"item":"intake.intent"`) || !strings.Contains(text, `"admitted":true`) {
			t.Fatalf("tools reply missing admission: %s", out.String())
		}

		// FA1 requires ONE decide hook per in-scope frame. Strict red check:
		// runToolsServer gates the frame, then toolsDecide builds a second
		// bridge and gates the same request again.
		out.Reset()
		before := stub.count()
		dbl := w25_frame("tools/call", 3,
			`"name":"system1_decide","arguments":{"item":"intake.intent","state":"classify this request","phase":"before-model"},`+w25_caps)
		runToolsServer(bytes.NewReader(append(dbl, '\n')), &out)
		w25_waitCount(t, stub, before+1)
		time.Sleep(50 * time.Millisecond)
		if got := stub.count() - before; got != 1 {
			t.Fatalf("BROKEN FA1: one tools/call frame produced %d SO POSTs, want 1 (two decide hooks). Fix: mcp_tools.go:78 decideFrame and mcp_tools.go:222 newDecisionBridge().decideFull must share one call", got)
		}
	})

	t.Run("dashboard", func(t *testing.T) {
		stub := w25_env(t, "advisory")
		srv := httptest.NewServer(NewDashboardHandler())
		defer srv.Close()
		w25_do(t, http.MethodGet, srv.URL+"/", "", "")
		w25_do(t, http.MethodPost, srv.URL+"/api/prompts",
			`{"name":"w25","text":"probe"}`, srv.URL)
		if stub.count() != 0 {
			t.Fatalf("dashboard SO POST count = %d, want 0 (stray SO call)", stub.count())
		}
	})
}

// FA2: inScope is bound to the JSON-RPC method.
func TestSimFA2MethodScopeAndNormalization(t *testing.T) {
	stub := w25_env(t, "advisory")
	cases := []struct {
		method     string
		state      string
		wantPost   bool
		wantReason string
	}{
		{"tools/call", "decide: fa2 tools call", true, "so-admit"},
		{"initialize", "decide: fa2 initialize", true, "so-admit"},
		{"resources/list", "decide: fa2 resources", false, "layer0-scope"},
		{"tools/list", "decide: fa2 tools list", false, "layer0-scope"},
		{"ping", "decide: fa2 ping", false, "layer0-scope"},
		{"notifications/initialized", "decide: fa2 notif", false, "layer0-scope"},
		{"TOOLS/CALL", "decide: fa2 upper", true, "so-admit"},
		{"tools_call", "decide: fa2 underscore", false, "layer0-scope"},
	}
	for i, c := range cases {
		before := stub.count()
		d := newDecisionBridge()
		r := d.decideFrame(w25_frame(c.method, i+1, fmt.Sprintf(`"state":%q,%s`, c.state, w25_caps)))
		if c.wantPost {
			w25_waitCount(t, stub, before+1)
		}
		if got := stub.count() - before; (got == 1) != c.wantPost {
			t.Fatalf("method %q: SO POST delta = %d, wantPost=%v", c.method, got, c.wantPost)
		}
		if r.Reason != c.wantReason {
			t.Fatalf("method %q: reason = %q, want %q", c.method, r.Reason, c.wantReason)
		}
	}
}

// FA2: phase and SO answer labels normalized before admission.
func TestSimFA2PhaseLabelNormalization(t *testing.T) {
	stub := w25_env(t, "advisory")
	phases := []struct {
		phase    string
		state    string
		wantPost bool
	}{
		{"before-model", "decide: phase one", true},
		{"before_model", "decide: phase two", true},
		{"BEFORE-MODEL", "decide: phase three", true},
		{"pre_model", "decide: phase four", true},
		{"PRE-MODEL", "decide: phase five", true},
		{"post_tool", "decide: phase six", false},
		{"after-model", "decide: phase seven", false},
		{"", "decide: phase eight", false},
	}
	for i, c := range phases {
		before := stub.count()
		d := newDecisionBridge()
		params := fmt.Sprintf(`"state":%q,"phase":%q,%s`, c.state, c.phase, w25_capsNoPhase)
		r := d.decideFrame(w25_frame("tools/call", 100+i, params))
		if c.wantPost {
			w25_waitCount(t, stub, before+1)
		}
		if got := stub.count() - before; (got == 1) != c.wantPost {
			t.Fatalf("phase %q: SO POST delta = %d, wantPost=%v", c.phase, got, c.wantPost)
		}
		if c.wantPost && r.Decision != decPin {
			t.Fatalf("phase %q: decision = %q reason = %q, want pin", c.phase, r.Decision, r.Reason)
		}
		if !c.wantPost && r.Reason != "layer0-scope" {
			t.Fatalf("phase %q: reason = %q, want layer0-scope", c.phase, r.Reason)
		}
	}

	// FA2 requires label normalization before the gate. Strict red check:
	// an upper-case SO label is currently rejected as outside the criteria set.
	stub.setReply(`{"model":"clef","answers":{"gate":{"type":"choice","choice":"YES","confidence":0.9,"probabilities":{"yes":0.9,"no":0.1}}}}`)
	d := newDecisionBridge()
	upper := d.decideFrame(w25_frame("tools/call", 200, `"state":"decide: uppercase label",`+w25_caps))
	if upper.Decision != decPin || upper.Reason != "so-admit" || upper.LastDecision.Choice != "yes" {
		t.Fatalf("BROKEN FA2: upper-case SO label gave decision=%q reason=%q choice=%q, want pin/so-admit/yes. Fix: gate.go:320-328 validSOAnswer must normalize a.Choice (TrimSpace+ToLower) against q.Criteria before membership test",
			upper.Decision, upper.Reason, upper.LastDecision.Choice)
	}
	stub.setReply(w25_yesReply)
	d2 := newDecisionBridge()
	lower := d2.decideFrame(w25_frame("tools/call", 201, `"state":"decide: lowercase label",`+w25_caps))
	if lower.Decision != decPin || lower.Reason != "so-admit" {
		t.Fatalf("lowercase label: decision=%q reason=%q, want pin/so-admit", lower.Decision, lower.Reason)
	}
}

// FA2: unknown or missing item falls back to the generic yes/no/other question.
func TestSimFA2CatchAllItem(t *testing.T) {
	stub := w25_env(t, "advisory")
	cases := []struct {
		name  string
		item  string
		state string
	}{
		{"missing item", "", "decide: catchall missing"},
		{"unknown item", "no.such.item", "decide: catchall unknown"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := stub.count()
			params := fmt.Sprintf(`"state":%q,`, c.state)
			if c.item != "" {
				params += fmt.Sprintf(`"item":%q,`, c.item)
			}
			params += w25_caps
			d := newDecisionBridge()
			r := d.decideFrame(w25_frame("tools/call", i+1, params))
			w25_waitCount(t, stub, before+1)
			if r.Decision != decPin || r.Reason != "so-admit" {
				t.Fatalf("decision=%q reason=%q, want pin/so-admit", r.Decision, r.Reason)
			}
			var req w25_soRequest
			if err := json.Unmarshal([]byte(stub.body(before)), &req); err != nil {
				t.Fatal(err)
			}
			if len(req.Questions) != 1 {
				t.Fatalf("SO questions = %d, want 1", len(req.Questions))
			}
			if _, ok := req.Questions["gate"]; !ok {
				t.Fatalf("SO question key missing gate: %s", stub.body(before))
			}
			var sk StateSkeleton
			if err := json.Unmarshal([]byte(req.State), &sk); err != nil {
				t.Fatal(err)
			}
			if sk.Item != "" {
				t.Fatalf("SO state item = %q, want empty (generic path)", sk.Item)
			}
			if sk.Method != "tools/call" || sk.Phase != "before-model" {
				t.Fatalf("SO state method=%q phase=%q, want tools/call/before-model", sk.Method, sk.Phase)
			}
		})
	}
}
