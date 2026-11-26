package app

// Simulation for MANIFEST FF1 and YC5. Every SO call goes to a loopback
// httptest stub. HOME is temporary, the key is a literal test value, no real
// credential is read and no production file is touched.

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	w33_Key    = "sim-test-key"
	w33_Secret = "api_key=SIM-SECRET-DO-NOT-SEND"
	w33_Caps   = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,` +
		`"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`
	w33_State = "classify sim ff1 " + w33_Secret
)

type w33_Hit struct {
	method string
	path   string
	key    string
	body   []byte
}

type w33_Stub struct {
	mu    sync.Mutex
	hits  []w33_Hit
	reply func([]byte) string
}

func (s *w33_Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.hits = append(s.hits, w33_Hit{r.Method, r.URL.Path, r.Header.Get("x-api-key"), body})
	reply := s.reply
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if reply == nil {
		_, _ = io.WriteString(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
		return
	}
	_, _ = io.WriteString(w, reply(body))
}

func w33_SO(t *testing.T, reply func([]byte) string) (*httptest.Server, *w33_Stub) {
	t.Helper()
	s := &w33_Stub{reply: reply}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func w33_SetReply(s *w33_Stub, reply func([]byte) string) {
	s.mu.Lock()
	s.reply = reply
	s.mu.Unlock()
}

func w33_Reply(choice string, conf float64) func([]byte) string {
	return func([]byte) string {
		return `{"answers":{"gate":{"type":"choice","choice":"` + choice + `","confidence":` +
			strconv.FormatFloat(conf, 'g', -1, 64) + `}}}`
	}
}

func w33_Count(s *w33_Stub) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

func w33_Wait(t *testing.T, s *w33_Stub, want int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if w33_Count(s) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("SO POST count = %d, want >= %d", w33_Count(s), want)
}

func w33_Quiet(t *testing.T, s *w33_Stub, want int) {
	t.Helper()
	fail := func(got int) {
		s.mu.Lock()
		var bodies [][]byte
		for _, h := range s.hits {
			bodies = append(bodies, h.body)
		}
		s.mu.Unlock()
		t.Fatalf("SO POST count = %d, want exactly %d; bodies=%q", got, want, bodies)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := w33_Count(s); got > want {
			fail(got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := w33_Count(s); got != want {
		fail(got)
	}
}

func w33_Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	return home
}

func w33_Attach(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint = srv.URL + "/v1/systemone"
	soClient = srv.Client()
	t.Setenv("EVREN_API_KEY", w33_Key)
	t.Cleanup(func() { soEndpoint, soClient = oldEndpoint, oldClient })
}

func w33_Envelope(state string, needLabel bool) string {
	e := stateEnvelope{
		State: state, Method: "tools/call", NeedLabel: needLabel, NoScratch: true,
		Safe: true, Reversible: true, OptIn: true, Phase: "before-model",
		Authority: true, Calibrated: true,
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func w33_Params(state string, caps bool) map[string]any {
	p := map[string]any{"state": state}
	if caps {
		_ = json.Unmarshal([]byte("{"+w33_Caps+"}"), &p)
	}
	return p
}

func w33_Frame(id int, state string, caps bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": w33_Params(state, caps),
	})
	return append(b, '\n')
}

func w33_ToolFrame(id int, name, state string) []byte {
	p := w33_Params(state, true)
	p["name"] = name
	p["arguments"] = map[string]any{"state": state, "phase": "before-model"}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": p,
	})
	return append(b, '\n')
}

type w33_Buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *w33_Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *w33_Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func w33_Build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "walsy-system1")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/walsy-system1")
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CGO_ENABLED=0 build failed: %v\n%s", err, out)
	}
	return bin
}

func w33_BinaryEnv(t *testing.T, endpoint string) []string {
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
		"WALSY_SO_MODE": true, "WALSY_SO_ENDPOINT": true, "EVREN_API_KEY": true,
		"MCP_PROXY_PORT": true, "WALSY_ROUTER_DENY": true,
	}
	env := make([]string, 0, len(os.Environ())+7)
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		if !skip[name] {
			env = append(env, kv)
		}
	}
	return append(env,
		"HOME="+home, "TMPDIR="+tmp, "WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT="+endpoint, "EVREN_API_KEY="+w33_Key)
}

func w33_Start(t *testing.T, bin string, env []string, args ...string) (*exec.Cmd, *w33_Buffer) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	errb := &w33_Buffer{}
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

func w33_Addr(t *testing.T, errb *w33_Buffer) string {
	t.Helper()
	needle := `"listen_addr":"`
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		s := errb.String()
		if i := strings.Index(s, needle); i >= 0 {
			rest := s[i+len(needle):]
			if j := strings.Index(rest, `"`); j > 0 {
				return rest[:j]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no listen_addr metric; stderr=%s", errb.String())
	return ""
}

// w33_Run keeps stdin open through hold so the advisory worker finishes
// before the process sees EOF; a straight stdin pipe can drop the POST.
func w33_Run(t *testing.T, cmd *exec.Cmd, stdin []byte, hold func()) ([]byte, string) {
	t.Helper()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &w33_Buffer{}
	errb := &w33_Buffer{}
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if len(stdin) > 0 {
		if _, err := in.Write(stdin); err != nil {
			t.Fatal(err)
		}
	}
	if hold != nil {
		hold()
	}
	_ = in.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exit: %v; stderr=%s", err, errb.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("binary timeout")
	}
	return []byte(out.String()), errb.String()
}

func w33_Unix(t *testing.T) (string, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "w33_sock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	up := filepath.Join(root, "up.sock")
	l, err := net.Listen("unix", up)
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
				_, _ = io.Copy(c, c) // real upstream echo
			}()
		}
	}()
	return filepath.Join(root, "in.sock"), up
}

func w33_PickPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr[strings.LastIndex(addr, ":")+1:]
}

func w33_GET(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func w33_CheckWire(t *testing.T, s *w33_Stub) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hits) == 0 {
		t.Fatal("no SO POST captured")
	}
	for i, h := range s.hits {
		if h.method != http.MethodPost || h.path != "/v1/systemone" {
			t.Errorf("hit %d = %s %s, want POST /v1/systemone", i, h.method, h.path)
		}
		if h.key != "" {
			t.Errorf("hit %d loopback x-api-key = %q, want none", i, h.key)
		}
		if !bytes.Contains(h.body, []byte(`"state"`)) {
			t.Errorf("hit %d body has no state: %s", i, h.body)
		}
		if !bytes.Contains(h.body, []byte(`"model":"clef"`)) {
			t.Errorf("hit %d body has no model=clef: %s", i, h.body)
		}
	}
}

// FF1(1): all five transports route through the same decision hook. A frame
// that reaches the hook causes exactly one POST; a layer-0 frame causes none.
func TestSimFF1TransportsSingleHook(t *testing.T) {
	srv, stub := w33_SO(t, nil)
	bin := w33_Build(t)
	env := w33_BinaryEnv(t, srv.URL+"/v1/systemone")
	frame := w33_Frame(1, w33_State, true)
	plain := w33_Frame(2, "status", true) // Layer 0 static pass: zero SO POST

	t.Run("stdio", func(t *testing.T) {
		before := w33_Count(stub)
		cmd := exec.Command(bin, "cat")
		cmd.Env = env
		out, _ := w33_Run(t, cmd, frame, func() {
			w33_Wait(t, stub, before+1)
		})
		if !bytes.Contains(out, []byte(`"id":1`)) {
			t.Fatalf("stdio echo missing: %s", out)
		}
		w33_Quiet(t, stub, before+1)
		cmd = exec.Command(bin, "cat")
		cmd.Env = env
		out, _ = w33_Run(t, cmd, plain, nil)
		if !bytes.Contains(out, []byte(`"id":2`)) {
			t.Fatalf("stdio layer0 echo missing: %s", out)
		}
		w33_Quiet(t, stub, before+1)
	})

	t.Run("unix", func(t *testing.T) {
		inPath, upPath := w33_Unix(t)
		before := w33_Count(stub)
		w33_Start(t, bin, env, "-transport", "unix", "-listen", inPath, "-upstream", upPath)
		deadline := time.Now().Add(6 * time.Second)
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
		for _, tc := range []struct {
			in   []byte
			want int
		}{{frame, before + 1}, {plain, before + 1}} {
			if _, err := c.Write(tc.in); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil {
				t.Fatalf("unix echo: %v", err)
			}
			if !bytes.Equal(bytes.TrimSpace(line), bytes.TrimSpace(tc.in)) {
				t.Fatalf("unix echo = %q, want %q", line, tc.in)
			}
			w33_Wait(t, stub, tc.want)
		}
		w33_Quiet(t, stub, before+1)
	})

	t.Run("http", func(t *testing.T) {
		before := w33_Count(stub)
		_, errb := w33_Start(t, bin, env, "-transport", "http", "-listen", "127.0.0.1:0")
		addr := w33_Addr(t, errb)
		for _, tc := range []struct {
			in   []byte
			want int
		}{{frame, before + 1}, {plain, before + 1}} {
			resp, err := http.Post("http://"+addr+"/", "application/json", bytes.NewReader(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200", resp.StatusCode)
			}
			if !bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(tc.in)) {
				t.Fatalf("HTTP echo = %q, want %q", body, tc.in)
			}
			w33_Wait(t, stub, tc.want)
		}
		w33_Quiet(t, stub, before+1)
	})

	t.Run("tools", func(t *testing.T) {
		before := w33_Count(stub)
		cmd := exec.Command(bin, "-transport", "tools")
		cmd.Env = env
		out, _ := w33_Run(t, cmd, w33_ToolFrame(3, "system1_status", w33_State), func() {
			w33_Wait(t, stub, before+1)
		})
		if !bytes.Contains(out, []byte(`"result"`)) {
			t.Fatalf("tools reply missing: %s", out)
		}
		w33_Quiet(t, stub, before+1)
	})

	t.Run("dashboard", func(t *testing.T) {
		before := w33_Count(stub)
		port := w33_PickPort(t)
		_, errb := w33_Start(t, bin, env, "-transport", "dashboard", "-listen", ":"+port)
		addr := "127.0.0.1:" + port
		ready := false
		for i := 0; i < 100; i++ {
			resp, err := http.Get("http://" + addr + "/")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ready = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ready {
			t.Fatalf("dashboard never ready; stderr=%s", errb.String())
		}
		if code := w33_GET(t, "http://"+addr+"/ui/prompts"); code != http.StatusOK {
			t.Fatalf("dashboard GET /ui/prompts = %d, want 200", code)
		}
		w33_Quiet(t, stub, before)
	})

	w33_CheckWire(t, stub)
}

// FF1(2),(3): enforcing blocks capability-less calls; advisory and low
// confidence stay fail-open; preflight is audit-only and never admits.
func TestSimFF1FailOpenPreflight(t *testing.T) {
	home := w33_Home(t)
	srv, stub := w33_SO(t, nil)
	w33_Attach(t, srv)

	t.Setenv("WALSY_SO_MODE", "enforcing")
	routerReset()
	d := newDecisionBridge()

	out, blocked := d.enforcingGate(w33_Frame(1, w33_State, false))
	if !blocked || !bytes.Contains(out, []byte("-32001")) ||
		!bytes.Contains(out, []byte("explicit decision capability")) {
		t.Fatalf("cap-missing enforcing reply = %s blocked=%v", out, blocked)
	}
	if got := w33_Count(stub); got != 0 {
		t.Fatalf("cap-missing POST count = %d, want 0", got)
	}

	routerReset()
	w33_SetReply(stub, w33_Reply("yes", 0.79))
	raw := w33_Frame(2, w33_State, true)
	out, blocked = d.enforcingGate(raw)
	if blocked || out != nil {
		t.Fatalf("low-confidence enforcing blocked: %s blocked=%v", out, blocked)
	}
	w33_Wait(t, stub, 1)
	r := d.decideFrame(raw)
	if r.Decision != decPass || r.Reason != "so-low-conf" {
		t.Fatalf("low-confidence route = %+v", r)
	}

	routerReset()
	w33_SetReply(stub, w33_Reply("yes", 0.95))
	raw = w33_Frame(3, "classify ff1 high "+w33_Secret, true)
	out, blocked = d.enforcingGate(raw)
	if blocked || out != nil {
		t.Fatalf("0.95 enforcing refused: %s blocked=%v", out, blocked)
	}
	w33_Wait(t, stub, 2)

	routerReset()
	t.Setenv("EVREN_API_KEY", "")
	d2 := newDecisionBridge()
	out, blocked = d2.enforcingGate(w33_Frame(4, "classify ff1 no key", true))
	if blocked || out != nil {
		t.Fatalf("no-key enforcing refused: %s blocked=%v", out, blocked)
	}
	if got := w33_Count(stub); got != 2 {
		t.Fatalf("no-key POST count = %d, want 2", got)
	}
	t.Setenv("EVREN_API_KEY", w33_Key)

	t.Setenv("WALSY_SO_MODE", "")
	if got := soMode(); got != "advisory" {
		t.Fatalf("default soMode = %q, want advisory", got)
	}
	routerReset()
	if out, blocked := d.enforcingGate(w33_Frame(5, "classify ff1 advisory", true)); blocked || out != nil {
		t.Fatalf("advisory gate blocked: %s blocked=%v", out, blocked)
	}

	amb := preflightFromState(w33_Envelope(w33_State, false))
	if !amb.Ambiguous {
		t.Fatalf("preflight ambiguity = %+v, want Ambiguous", amb)
	}
	before := w33_Count(stub)
	r = route(w33_Envelope(w33_State, false), d.pin)
	if r.Decision != decPass || r.Reason != "layer0-gate" {
		t.Fatalf("preflight-ambiguous route = %+v, want a deterministic pass", r)
	}
	if got := w33_Count(stub); got != before {
		t.Fatalf("preflight-ambiguous POST count = %d, want %d", got, before)
	}
	audit := w33_Read(t, filepath.Join(home, routerDir, routerAuditName))
	if !strings.Contains(audit, `"source":"preflight"`) {
		t.Fatal("audit has no source=preflight record")
	}
}

func w33_Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// FF1(4),(6),(7): canonical state caches, concurrent keys coalesce, a prompt
// set change namespaces the cache, audit drops secrets and raw confidence,
// and receipts chain by nonce/prev/hash.
func TestSimFF1CachePolicyAndReceipts(t *testing.T) {
	home := w33_Home(t)
	srv, stub := w33_SO(t, func([]byte) string {
		time.Sleep(30 * time.Millisecond)
		return w33_Reply("yes", 0.95)(nil)
	})
	w33_Attach(t, srv)
	routerReset()

	d := newDecisionBridge()
	d.pin.markSuccess(d.pin.current().Name) // stable engine identity
	frame := w33_Frame(1, w33_State, true)
	r1 := d.decideFrame(frame)
	if r1.Decision != decPin || r1.Reason != "so-admit" || r1.ConfBucket != "0.9" {
		t.Fatalf("first route = %+v", r1)
	}
	w33_Wait(t, stub, 1)
	r2 := d.decideFrame(w33_Frame(99, w33_State, true))
	if r2.Decision != decPin || r2.Reason != "cache-hit" {
		t.Fatalf("second route = %+v, want cache-hit", r2)
	}
	w33_Quiet(t, stub, 1)

	d2 := newDecisionBridge()
	d2.pin.markSuccess(d2.pin.current().Name)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = d2.decideFrame(w33_Frame(200+i, w33_State, true))
		}(i)
	}
	wg.Wait()
	w33_Wait(t, stub, 2)
	w33_Quiet(t, stub, 2)

	baseKey := gateKey(frame, soCapability())
	if err := savePrompts([]Prompt{{Name: "w33_-bump", Text: "sim policy bump"}}); err != nil {
		t.Fatal(err)
	}
	bumpedKey := gateKey(frame, soCapability())
	if baseKey == bumpedKey {
		t.Fatalf("prompt-set change did not namespace gate key: %s", baseKey)
	}
	d3 := newDecisionBridge()
	d3.pin.markSuccess(d3.pin.current().Name)
	r3 := d3.decideFrame(frame)
	if r3.Decision != decPin || r3.Reason != "so-admit" {
		t.Fatalf("post-bump route = %+v", r3)
	}
	w33_Wait(t, stub, 3)

	audit := w33_Read(t, filepath.Join(home, routerDir, routerAuditName))
	for _, forbidden := range []string{w33_Secret, w33_Key, `"confidence"`, "0.95"} {
		if strings.Contains(audit, forbidden) {
			t.Errorf("audit leaked %q", forbidden)
		}
	}

	chain := w33_Read(t, filepath.Join(home, routerDir, "receipt.chain"))
	lines := strings.Split(strings.TrimSpace(chain), "\n")
	if len(lines) == 0 {
		t.Fatal("empty receipt chain")
	}
	prev := make([]byte, 0)
	nonces := map[string]bool{}
	for i, line := range lines {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("receipt %d JSON: %v", i, err)
		}
		var seq int
		_ = json.Unmarshal(m["seq"], &seq)
		if seq != i+1 {
			t.Errorf("receipt %d seq = %d, want %d", i, seq, i+1)
		}
		wantPrev := `"` + hex.EncodeToString(prev) + `"`
		if string(m["prev"]) != wantPrev {
			t.Errorf("receipt %d prev = %s, want %s", i, m["prev"], wantPrev)
		}
		var nonce string
		_ = json.Unmarshal(m["nonce"], &nonce)
		if nonce == "" || nonces[nonce] {
			t.Errorf("receipt %d nonce = %q", i, nonce)
		}
		nonces[nonce] = true
		if string(m["response_id"]) != `"none"` {
			t.Errorf("receipt %d response_id = %s, want none", i, m["response_id"])
		}
		sum := chainHash(prev, bytes.TrimSuffix([]byte(line), []byte("\n")))
		prev = sum[:]
	}
	// FF1(7) frame id: observed here, not asserted, because the manifest line
	// is ambiguous about whether it belongs in the chain record or the audit.
	t.Logf("receipt fields: %s", strings.Join(w33_Keys(lines[0]), ","))
}

func w33_Keys(line string) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// YC5: validate the whole answer, never re-argmax the probability map.
func TestSimYC5WholeAnswerNoReArgmax(t *testing.T) {
	q := soQuestion{Type: "choice", Criteria: map[string]string{
		"yes": "admit", "no": "pass", "other": "pass",
	}}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"declared-winner", `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9,"probabilities":{"no":0.99,"yes":0.01}}}}`, "yes"},
		{"score-type", `{"answers":{"gate":{"type":"score","choice":"yes","confidence":0.9,"score":0.9,"probabilities":{"yes":0.9,"no":0.1}}}}`, ""},
		{"missing-type", `{"answers":{"gate":{"choice":"yes","confidence":0.9}}}`, ""},
		{"bad-label", `{"answers":{"gate":{"type":"choice","choice":"maybe","confidence":0.9}}}`, ""},
		{"confidence-above-one", `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":1.2}}}`, ""},
		{"duplicate-key", `{"answers":{"gate":{"type":"choice","choice":"yes","choice":"no","confidence":0.9}}}`, ""},
		{"unknown-top-level", `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9}},"debug":true}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w33_Home(t)
			srv, _ := w33_SO(t, func([]byte) string { return tc.body })
			w33_Attach(t, srv)
			routerReset()
			d := askSOQ("decide: classify this", q)
			if d.Choice != tc.want {
				t.Fatalf("choice = %q, want %q", d.Choice, tc.want)
			}
			if tc.name == "declared-winner" {
				if d.Trace.Winner != "yes" || d.Trace.RunnerUp != "no" || !d.Trace.Comparable || d.Trace.Reason != "" {
					t.Fatalf("ranking trace = %+v, want declared winner yes, runner-up no", d.Trace)
				}
			}
		})
	}

	t.Run("route-keeps-declared-label", func(t *testing.T) {
		home := w33_Home(t)
		srv, _ := w33_SO(t, func([]byte) string {
			return `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95,"probabilities":{"no":0.99,"yes":0.01}}}}`
		})
		w33_Attach(t, srv)
		routerReset()
		r := route(w33_Envelope("classify yc5 decide", true), &pin{})
		if r.Decision != decPin || r.Reason != "so-admit" {
			t.Fatalf("route = %+v, want pin/so-admit", r)
		}
		if r.LastDecision.Choice != "yes" || r.LastDecision.Trace.Winner != "yes" {
			t.Fatalf("route answer re-argmaxed: %+v", r.LastDecision)
		}
		audit := w33_Read(t, filepath.Join(home, routerDir, routerAuditName))
		if strings.Contains(audit, `"confidence"`) || strings.Contains(audit, "0.95") {
			t.Fatal("audit leaked raw confidence")
		}
	})
}
