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
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	w03_SOOK        = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.99,"probabilities":{"yes":0.99,"no":0.01}}},"usage":{"credits":2}}`
	w03_promptBody  = "SECRETPROMPTBODY-DO-NOT-SEND"
	w03_testKey     = "sim-test-key"
	w03_maxSOState  = 4096
	w03_deadlineCap = 250 * time.Millisecond
)

type w03_SOPost struct {
	body   []byte
	key    string
	remote string
}

type w03_SOStub struct {
	srv         *httptest.Server
	mu          sync.Mutex
	posts       []w03_SOPost
	delay       time.Duration
	inFlight    int
	maxInFlight int
}

func w03_newSOStub() *w03_SOStub {
	s := &w03_SOStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.inFlight++
		if s.inFlight > s.maxInFlight {
			s.maxInFlight = s.inFlight
		}
		s.posts = append(s.posts, w03_SOPost{body: body, key: r.Header.Get("x-api-key"), remote: r.RemoteAddr})
		d := s.delay
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inFlight--
			s.mu.Unlock()
		}()
		if d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, w03_SOOK)
	}))
	return s
}

func (s *w03_SOStub) setDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func (s *w03_SOStub) reset() {
	s.mu.Lock()
	s.posts = nil
	s.maxInFlight = 0
	s.mu.Unlock()
}

func (s *w03_SOStub) snapshot() ([]w03_SOPost, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]w03_SOPost(nil), s.posts...), s.maxInFlight
}

type w03_syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *w03_syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *w03_syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func w03_home(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	prompts := `[{"name":"sim","text":"` + w03_promptBody + `"}]`
	if err := os.WriteFile(filepath.Join(dir, "prompts.json"), []byte(prompts), 0o600); err != nil {
		t.Fatalf("write prompts: %v", err)
	}
	return home
}

func w03_env(home, endpoint string) []string {
	drop := map[string]bool{
		"HOME": true, "TMPDIR": true, "WALSY_SO_MODE": true, "WALSY_SO_ENDPOINT": true,
		"EVREN_API_KEY": true, "NO_PROXY": true, "no_proxy": true,
		"HTTP_PROXY": true, "http_proxy": true, "HTTPS_PROXY": true, "https_proxy": true,
		"ALL_PROXY": true, "all_proxy": true,
	}
	env := make([]string, 0, len(os.Environ())+10)
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		if drop[key] {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+home,
		"TMPDIR="+home,
		"WALSY_SO_MODE=",
		"WALSY_SO_ENDPOINT="+endpoint,
		"EVREN_API_KEY="+w03_testKey,
		"NO_PROXY=*",
		"no_proxy=*",
		"CGO_ENABLED=0",
	)
}

func w03_build(t *testing.T, home, endpoint string) string {
	t.Helper()
	bin := filepath.Join(home, "walsy-system1")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = w03_env(home, endpoint)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func w03_frame(id int, state string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"system1_status","arguments":{},"state":%q,"phase":"before-model","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"method":"tools/call"}}`, id, state))
}

func w03_postMCP(client *http.Client, url string, frame []byte) (int, time.Duration, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		return 0, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(start), nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, time.Since(start), body, err
}

func w03_get(client *http.Client, url string) (int, []byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func w03_spawnHTTP(t *testing.T, bin, home, endpoint, upstream string) string {
	t.Helper()
	cmd := exec.Command(bin, "-transport", "http", "-upstream", upstream)
	stderr := &w03_syncBuffer{}
	cmd.Stderr = stderr
	cmd.Env = w03_env(home, endpoint)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn http binary: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	re := regexp.MustCompile(`"listen_addr":"([^"]+)"`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(stderr.String()); len(m) == 2 {
			return "http://" + m[1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no listen_addr in stderr: %s", stderr.String())
	return ""
}

type w03_tools struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func w03_spawnTools(t *testing.T, bin, home, endpoint string) *w03_tools {
	t.Helper()
	cmd := exec.Command(bin, "-transport", "tools")
	cmd.Env = w03_env(home, endpoint)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn tools binary: %v", err)
	}
	c := &w03_tools{cmd: cmd, in: in, out: bufio.NewReader(out)}
	t.Cleanup(func() { c.stop() })
	return c
}

func (c *w03_tools) stop() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.in.Close()
	_ = c.cmd.Wait()
}

func (c *w03_tools) roundTrip(frame []byte) ([]byte, error) {
	if _, err := c.in.Write(append(append([]byte(nil), frame...), '\n')); err != nil {
		return nil, err
	}
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		ch <- result{line: line, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return bytes.TrimSpace(r.line), nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("tools reply timeout")
	}
}

func w03_spawnDashboard(t *testing.T, bin, home, endpoint string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dashboard port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	cmd := exec.Command(bin, "-transport", "dashboard", "-listen", ":"+strconv.Itoa(port))
	cmd.Env = w03_env(home, endpoint)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn dashboard binary: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := w03_get(client, base+"/api/health"); err == nil {
			return base
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dashboard did not start at %s", base)
	return ""
}

func w03_audit(home string) string {
	data, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		return ""
	}
	return string(data)
}

func w03_receiptLines(t *testing.T, home string) []string {
	t.Helper()
	path := filepath.Join(home, ".walsy-system1", "receipt.chain")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read receipt chain: %v", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func w03_echoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func TestSimF5(t *testing.T) {
	stub := w03_newSOStub()
	defer stub.srv.Close()
	upstream := w03_echoUpstream(t)
	defer upstream.Close()

	home := w03_home(t, "so_mode=advisory\n")
	bin := w03_build(t, home, stub.srv.URL)
	mcp := w03_spawnHTTP(t, bin, home, stub.srv.URL, upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}

	frame1 := w03_frame(1, "classify sim state one")
	status, _, _, err := w03_postMCP(client, mcp, frame1)
	if err != nil || status != http.StatusOK {
		t.Fatalf("first frame: status=%d err=%v", status, err)
	}
	posts, _ := stub.snapshot()
	if len(posts) != 1 {
		t.Fatalf("SO POST count = %d, want 1", len(posts))
	}
	var wire struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(posts[0].body, &wire); err != nil {
		t.Fatalf("SO body is not JSON: %v", err)
	}
	if wire.Model != "clef" {
		t.Errorf("SO model = %q, want clef", wire.Model)
	}
	if _, ok := wire.Questions["gate"]; !ok || len(wire.Questions) != 1 {
		t.Errorf("SO questions = %d entries, want one gate object", len(wire.Questions))
	}
	if len(wire.State) > w03_maxSOState {
		t.Errorf("SO state = %d bytes, want <= %d", len(wire.State), w03_maxSOState)
	}
	if !strings.Contains(wire.State, `"sha256"`) {
		t.Errorf("SO state has no prompt digest: %s", wire.State)
	}
	if bytes.Contains(posts[0].body, []byte(w03_promptBody)) {
		t.Error("SO POST leaked a prompt body")
	}
	if posts[0].key != w03_testKey {
		t.Errorf("SO x-api-key = %q, want hermetic test key", posts[0].key)
	}
	audit := w03_audit(home)
	for _, want := range []string{`"event":"call"`, `"latency_ms"`, `"credits":"2"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit.jsonl missing %s (bytes=%d)", want, len(audit))
		}
	}
	for _, leak := range []string{w03_testKey, w03_promptBody, `"choice"`, `"confidence"`, `"probabilities"`} {
		if strings.Contains(audit, leak) {
			t.Errorf("audit.jsonl leaked %q (bytes=%d)", leak, len(audit))
		}
	}

	frame2 := w03_frame(2, "classify sim state two")
	status, _, _, err = w03_postMCP(client, mcp, frame2)
	if err != nil || status != http.StatusOK {
		t.Fatalf("second frame: status=%d err=%v", status, err)
	}
	posts, _ = stub.snapshot()
	if len(posts) != 2 {
		t.Fatalf("SO POST count = %d, want 2", len(posts))
	}
	if posts[1].remote != posts[0].remote {
		t.Errorf("SO connections differ: %q then %q (keep-alive not reused)", posts[0].remote, posts[1].remote)
	}

	status, _, _, err = w03_postMCP(client, mcp, frame1)
	if err != nil || status != http.StatusOK {
		t.Fatalf("cache replay frame: status=%d err=%v", status, err)
	}
	posts, _ = stub.snapshot()
	if len(posts) != 2 {
		t.Errorf("SO POST count after replay = %d, want 2 (cache hit)", len(posts))
	}

	stub.setDelay(400 * time.Millisecond)
	deadlineFrame := w03_frame(3, "classify sim state deadline")
	_, elapsed, _, err := w03_postMCP(client, mcp, deadlineFrame)
	if err != nil {
		t.Fatalf("deadline frame: %v", err)
	}
	if elapsed >= w03_deadlineCap {
		t.Errorf("400ms SO response took %v, want < %v (150ms deadline)", elapsed, w03_deadlineCap)
	}
	stub.setDelay(0)

	stub.reset()
	stub.setDelay(150 * time.Millisecond)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			frame := w03_frame(100+n, fmt.Sprintf("classify sim parallel %d", n))
			st, _, _, e := w03_postMCP(client, mcp, frame)
			if e != nil || st != http.StatusOK {
				errs <- fmt.Errorf("parallel frame %d: status=%d err=%v", n, st, e)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	posts, maxInFlight := stub.snapshot()
	// Bounded concurrency: N28/T1 allow 2 simultaneous SO POSTs; never more.
	// Redaction checks above still reject any secret/sensitive data in those posts.
	if maxInFlight > 2 {
		t.Errorf("parallel SO POST max in-flight = %d, want <=2 (bounded concurrency cap)", maxInFlight)
	}
	for i, p := range posts {
		if len(p.body) == 0 || p.body[0] != '{' {
			t.Errorf("SO POST %d is not a single object: %q", i, p.body)
		}
	}
}

func TestSimF6(t *testing.T) {
	stub := w03_newSOStub()
	defer stub.srv.Close()
	upstream := w03_echoUpstream(t)
	defer upstream.Close()

	home := w03_home(t, "so_mode=advisory\nskip_receipts=true\nadmit_baseline=0.2\ndrift_tolerance=0.1\n")
	bin := w03_build(t, home, stub.srv.URL)
	client := &http.Client{Timeout: 5 * time.Second}

	base := w03_spawnDashboard(t, bin, home, stub.srv.URL)
	status, healthBody, err := w03_get(client, base+"/api/health")
	if err != nil || status != http.StatusOK {
		t.Fatalf("health: status=%d err=%v", status, err)
	}
	var health map[string]any
	if err := json.Unmarshal(healthBody, &health); err != nil {
		t.Fatalf("health is not JSON: %v", err)
	}
	for _, key := range []string{"ok", "build_sha", "vcs_modified", "items_sha256", "receipt_sha256", "so_mode", "so_endpoint_host"} {
		if _, ok := health[key]; !ok {
			t.Errorf("health fingerprint missing %s", key)
		}
	}
	if got := health["so_mode"]; got != "advisory" {
		t.Errorf("health so_mode = %v, want advisory from config", got)
	}
	if got := health["so_endpoint_host"]; got != strings.TrimPrefix(stub.srv.URL, "http://") {
		t.Errorf("health so_endpoint_host = %v, want %s", got, strings.TrimPrefix(stub.srv.URL, "http://"))
	}
	for _, key := range []string{"items_sha256", "receipt_sha256"} {
		if got, _ := health[key].(string); len(got) != 64 {
			t.Errorf("health %s = %q, want 64 hex chars", key, got)
		}
	}
	_, again, err := w03_get(client, base+"/api/health")
	if err != nil || !bytes.Equal(again, healthBody) {
		t.Errorf("health fingerprint changed: %q then %q (%v)", healthBody, again, err)
	}

	status, page, err := w03_get(client, base+"/")
	if err != nil || status != http.StatusOK {
		t.Fatalf("dashboard home: status=%d err=%v", status, err)
	}
	lowerPage := strings.ToLower(string(page))
	if !strings.Contains(lowerPage, "feedback") {
		t.Error("dashboard home has no feedback widget")
	}
	if !strings.Contains(lowerPage, "readonly") && !strings.Contains(lowerPage, "disabled") {
		t.Error("feedback widget is not read-only")
	}
	fbReq, err := http.NewRequest(http.MethodPost, base+"/api/feedback", strings.NewReader(`{"text":"sim"}`))
	if err != nil {
		t.Fatalf("feedback request: %v", err)
	}
	fbReq.Header.Set("Content-Type", "application/json")
	fbReq.Header.Set("Origin", base)
	fbResp, err := client.Do(fbReq)
	if err != nil {
		t.Fatalf("feedback POST: %v", err)
	}
	_ = fbResp.Body.Close()
	if fbResp.StatusCode >= 200 && fbResp.StatusCode < 300 {
		t.Errorf("feedback POST accepted with status %d (widget must be read-only)", fbResp.StatusCode)
	}

	stub.setDelay(300 * time.Millisecond)
	nbMCP := w03_spawnHTTP(t, bin, home, stub.srv.URL, upstream.URL)
	_, nbElapsed, _, err := w03_postMCP(client, nbMCP, w03_frame(9000, "classify sim nonblocking"))
	if err != nil {
		t.Fatalf("non-blocking frame: %v", err)
	}
	if nbElapsed >= 150*time.Millisecond {
		t.Errorf("reply blocked %v on SO/receipt work, want < 150ms", nbElapsed)
	}
	stub.setDelay(0)

	tools := w03_spawnTools(t, bin, home, stub.srv.URL)
	reply, err := tools.roundTrip(w03_frame(1, "classify sim receipt one"))
	if err != nil || !json.Valid(reply) {
		t.Fatalf("tools frame: reply=%q err=%v", reply, err)
	}
	if lines := w03_receiptLines(t, home); len(lines) != 0 {
		t.Errorf("skip_receipts=true still wrote %d receipt line(s)", len(lines))
	}
	if _, err := tools.roundTrip([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)); err != nil {
		t.Fatalf("ping frame: %v", err)
	}
	if lines := w03_receiptLines(t, home); len(lines) != 0 {
		t.Errorf("layer0 frame wrote %d receipt line(s)", len(lines))
	}

	for i := 0; i < 100; i++ {
		frame := w03_frame(1000+i, fmt.Sprintf("classify sim drift %d", i))
		if _, err := tools.roundTrip(frame); err != nil {
			t.Fatalf("drift frame %d: %v", i, err)
		}
	}
	tools.stop()
	audit := w03_audit(home)
	for _, want := range []string{`"event":"admit_rate"`, `"event":"drift_alarm"`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit.jsonl missing %s after 100 admits (bytes=%d)", want, len(audit))
		}
	}
}
