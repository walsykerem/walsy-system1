package main

// Simulation-only tests for MANIFEST F7 and N1. Everything runs against a
// freshly built binary and a local httptest System One stub. No live SO call,
// no key material read, no production file is modified.

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	w04_soKey       = "sim-test-key"
	w04_caps        = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`
	w04_secretValue = "api_key=SIM-SECRET-DO-NOT-SEND"
	w04_stateHigh   = "classify sim high " + w04_secretValue
	w04_stateLow    = "classify sim low " + w04_secretValue
	w04_stateSingle = "classify sim single door"
)

type w04_SORecord struct {
	method string
	key    string
	body   []byte
}

type w04_recorder struct {
	mu      sync.Mutex
	records []w04_SORecord
	conf    float64
}

func (s *w04_recorder) setConf(c float64) {
	s.mu.Lock()
	s.conf = c
	s.mu.Unlock()
}

func (s *w04_recorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *w04_recorder) all() []w04_SORecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]w04_SORecord(nil), s.records...)
}

// w04_soStub answers the item question with "build" and the generic gate question
// with "yes". Both are labels the shipped registry accepts.
func w04_soStub(t *testing.T, conf float64) (*httptest.Server, *w04_recorder) {
	t.Helper()
	rec := &w04_recorder{conf: conf}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.mu.Lock()
		rec.records = append(rec.records, w04_SORecord{
			method: r.Method,
			key:    r.Header.Get("x-api-key"),
			body:   body,
		})
		c := rec.conf
		rec.mu.Unlock()
		choice := "yes"
		if bytes.Contains(body, []byte(`"build"`)) {
			choice = "build"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"answers":{"gate":{"type":"choice","choice":%q,"confidence":%s}}}`,
			choice, strconv.FormatFloat(c, 'g', -1, 64))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func w04_waitCalls(t *testing.T, rec *w04_recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if rec.count() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("SO calls = %d, want >= %d", rec.count(), want)
}

func w04_quiet(t *testing.T, rec *w04_recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := rec.count(); got > want {
			t.Fatalf("SO calls = %d, want exactly %d", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rec.count(); got != want {
		t.Fatalf("SO calls = %d, want exactly %d", got, want)
	}
}

type w04_buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *w04_buffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *w04_buffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func w04_build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "walsy-system1")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func w04_env(t *testing.T, endpoint string) ([]string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{home, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	skip := map[string]bool{
		"HOME": true, "TMPDIR": true, "XDG_CONFIG_HOME": true,
		"WALSY_SO_MODE": true, "WALSY_SO_ENDPOINT": true,
		"EVREN_API_KEY": true, "MCP_PROXY_PORT": true,
	}
	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		if !skip[name] {
			env = append(env, kv)
		}
	}
	env = append(env,
		"HOME="+home,
		"TMPDIR="+tmp,
		"WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT="+endpoint,
		"EVREN_API_KEY="+w04_soKey,
	)
	return env, root
}

func w04_params(caps bool) map[string]any {
	p := map[string]any{}
	if caps {
		_ = json.Unmarshal([]byte("{"+w04_caps+"}"), &p)
	}
	return p
}

func w04_frame(id int, method, state string, caps bool) []byte {
	p := w04_params(caps)
	if state != "" {
		p["state"] = state
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p})
	return append(b, '\n')
}

func w04_toolFrame(id int, name, state string, decide bool) []byte {
	p := w04_params(true)
	p["name"] = name
	p["state"] = state // advisory hook sees the same label-shaped state
	args := map[string]any{"state": state, "phase": "before-model"}
	if decide {
		args["item"] = "intake.intent"
	} else {
		args["limit"] = 1
	}
	p["arguments"] = args
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": p})
	return append(b, '\n')
}

func w04_run(t *testing.T, cmd *exec.Cmd, stdin []byte) ([]byte, string) {
	t.Helper()
	cmd.Stdin = bytes.NewReader(stdin)
	out := &w04_buffer{}
	errb := &w04_buffer{}
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exit: %v; stderr=%s", err, errb.String())
		}
	case <-time.After(12 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("binary timeout")
	}
	return []byte(out.String()), errb.String()
}

func w04_runTools(t *testing.T, bin string, env []string, frames ...[]byte) [][]byte {
	t.Helper()
	cmd := exec.Command(bin, "-transport", "tools")
	cmd.Env = env
	out, _ := w04_run(t, cmd, bytes.Join(frames, nil))
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	res := make([][]byte, 0, len(lines))
	for _, line := range lines {
		if len(line) > 0 {
			res = append(res, line)
		}
	}
	return res
}

func w04_runStdio(t *testing.T, bin string, env []string, frames ...[]byte) []byte {
	t.Helper()
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, cat)
	cmd.Env = env
	out, _ := w04_run(t, cmd, bytes.Join(frames, nil))
	return out
}

func w04_start(t *testing.T, bin string, env []string, args ...string) (*exec.Cmd, *w04_buffer) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	errb := &w04_buffer{}
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd, errb
}

func w04_runUnix(t *testing.T, bin string, env []string, frame []byte) []byte {
	t.Helper()
	// macOS sun_path is 104 bytes; keep this short and outside t.TempDir().
	root, err := os.MkdirTemp("", "w04sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	upPath := filepath.Join(root, "u.sock")
	l, err := net.Listen("unix", upPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c) // upstream echo, real socket transport
			}()
		}
	}()
	inPath := filepath.Join(root, "i.sock")
	_, _ = w04_start(t, bin, env, "-transport", "unix", "-listen", inPath, "-upstream", upPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(inPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unix socket not created")
		}
		time.Sleep(20 * time.Millisecond)
	}
	c, err := net.Dial("unix", inPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(frame); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("unix reply: %v", err)
	}
	return line
}

func w04_runHTTP(t *testing.T, bin string, env []string, frame []byte) []byte {
	t.Helper()
	_, errb := w04_start(t, bin, env, "-transport", "http", "-listen", "127.0.0.1:0")
	addr := ""
	needle := `"listen_addr":"`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := errb.String()
		if i := strings.Index(s, needle); i >= 0 {
			rest := s[i+len(needle):]
			if j := strings.Index(rest, `"`); j > 0 {
				addr = rest[:j]
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("no listen_addr metric")
	}
	resp, err := http.Post("http://"+addr+"/", "application/json", bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200", resp.StatusCode)
	}
	return body
}

func w04_replyText(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("reply JSON: %v (%s)", err, line)
	}
	if len(env.Result.Content) == 0 {
		t.Fatalf("no tool content in reply: %s", line)
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &text); err != nil {
		t.Fatalf("tool text JSON: %v", err)
	}
	return text
}

func w04_soState(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("SO body JSON: %v", err)
	}
	return req.State
}

// TestSimF7SOGateThreshold drives the real tools transport against a stubbed SO
// and checks the 0.8 admission cut and the wire shape of every SO POST.
func TestSimF7SOGateThreshold(t *testing.T) {
	srv, rec := w04_soStub(t, 0.95)
	bin := w04_build(t)
	env, _ := w04_env(t, srv.URL+"/v1/systemone")

	high := w04_runTools(t, bin, env, w04_toolFrame(1, "system1_decide", w04_stateHigh, true))
	w04_waitCalls(t, rec, 1) // single decide hook per frame
	w04_quiet(t, rec, 1)     // strict: no second call for one frame
	if len(high) != 1 {
		t.Fatalf("tools replies = %d, want 1: %q", len(high), high)
	}
	hi := w04_replyText(t, high[0])
	if hi["admitted"] != true || hi["label"] != "build" || hi["confidence"] != 0.95 {
		t.Fatalf("conf 0.95 reply = %v, want admitted build at 0.95", hi)
	}

	rec.setConf(0.79)
	low := w04_runTools(t, bin, env, w04_toolFrame(2, "system1_decide", w04_stateLow, true))
	w04_waitCalls(t, rec, 2)
	w04_quiet(t, rec, 2)
	if len(low) != 1 {
		t.Fatalf("low tools replies = %d, want 1: %q", len(low), low)
	}
	lo := w04_replyText(t, low[0])
	if lo["admitted"] != false || lo["confidence"] != 0.79 {
		t.Fatalf("conf 0.79 reply = %v, want not admitted at 0.79", lo)
	}
	if _, ok := lo["steer"]; !ok {
		t.Fatalf("conf 0.79 reply has no steer: %v", lo)
	}

	for i, c := range rec.all() {
		if c.method != http.MethodPost {
			t.Errorf("call %d method = %q, want POST", i, c.method)
		}
		if c.key != w04_soKey {
			t.Errorf("call %d x-api-key = %q, want stub key", i, c.key)
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(c.body, &req); err != nil {
			t.Fatalf("call %d SO body JSON: %v", i, err)
		}
		if req.Model != "clef" {
			t.Errorf("call %d model = %q, want clef", i, req.Model)
		}
		state := w04_soState(t, c.body)
		if !strings.Contains(state, "classify sim") {
			t.Errorf("call %d state = %q, missing label shape", i, state)
		}
		if strings.Contains(state, "SIM-SECRET-DO-NOT-SEND") {
			t.Errorf("call %d state leaked secret: %q", i, state)
		}
	}
}

// TestSimN1SingleDecisionDoor sends one in-scope frame through every JSON-RPC
// transport and checks one bounded SO POST with identical canonical state per
// frame, zero posts for out-of-scope frames, and a rogue-call-free dashboard.
func TestSimN1SingleDecisionDoor(t *testing.T) {
	srv, rec := w04_soStub(t, 0.95)
	bin := w04_build(t)
	env, _ := w04_env(t, srv.URL+"/v1/systemone")
	frame := w04_frame(10, "initialize", w04_stateSingle, true)

	w04_runStdio(t, bin, env, frame)
	w04_waitCalls(t, rec, 1)
	w04_quiet(t, rec, 1)
	w04_runUnix(t, bin, env, frame)
	w04_waitCalls(t, rec, 2)
	w04_quiet(t, rec, 2)
	w04_runHTTP(t, bin, env, frame)
	w04_waitCalls(t, rec, 3)
	w04_quiet(t, rec, 3)
	w04_runTools(t, bin, env, frame)
	w04_waitCalls(t, rec, 4)
	w04_quiet(t, rec, 4)

	states := make([]string, 0, 4)
	for _, c := range rec.all() {
		states = append(states, w04_soState(t, c.body))
	}
	for i := 1; i < len(states); i++ {
		if states[i] != states[0] {
			t.Fatalf("transport %d state differs:\n%s\n%s", i, states[0], states[i])
		}
	}
	var envelope struct {
		NeedLabel  bool   `json:"need_label"`
		NoScratch  bool   `json:"no_scratch"`
		Safe       bool   `json:"safe"`
		Reversible bool   `json:"reversible"`
		Method     string `json:"method"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal([]byte(states[0]), &envelope); err != nil {
		t.Fatalf("state envelope JSON: %v", err)
	}
	if !envelope.NeedLabel || !envelope.NoScratch || !envelope.Safe || !envelope.Reversible ||
		envelope.Method != "initialize" || envelope.State != w04_stateSingle {
		t.Fatalf("state envelope = %+v, want full opt-ins and initialize", envelope)
	}

	out := w04_frame(11, "tools/list", "", false)
	before := rec.count()
	w04_runStdio(t, bin, env, out)
	w04_runUnix(t, bin, env, out)
	w04_runHTTP(t, bin, env, out)
	w04_runTools(t, bin, env, out)
	w04_quiet(t, rec, before)

	before = rec.count()
	w04_runTools(t, bin, env,
		w04_toolFrame(20, "system1_status", w04_stateSingle, false),
		w04_toolFrame(21, "system1_audit", w04_stateSingle, false))
	w04_waitCalls(t, rec, before+2)
	w04_quiet(t, rec, before+2)

	// Dashboard: serve real handler, assert it never opens a rogue SO path.
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", srv.URL+"/v1/systemone")
	t.Setenv("EVREN_API_KEY", w04_soKey)
	dash := httptest.NewServer(NewDashboardHandler())
	defer dash.Close()
	resp, err := http.Get(dash.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard health = %d, want 200", resp.StatusCode)
	}
	before = rec.count()
	resp, err = http.Post(dash.URL+"/", "application/json", bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dashboard root status = %d, want 400 (method not allowed)", resp.StatusCode)
	}
	w04_quiet(t, rec, before)

	// N1's dashboard clause cannot be positively proven: dashboard.go has no
	// decision hook and no JSON-RPC frame route. Recorded in W04.md for parent.
	t.Log("dashboard: no decision hook; only no-rogue-SO behavior verified")
}
