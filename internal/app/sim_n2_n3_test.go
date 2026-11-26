package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Simulation of MANIFEST N2/N3 against the real MCP HTTP handler, the real
// decision bridge, the real SO wire parser and the shipped binary. No external
// SO endpoint is contacted: every POST lands on a loopback httptest server.
// A verdict below WORKS fails the test; a red test is evidence of a real gap.

type w05_Verdict struct {
	id      string
	verdict string
	detail  string
}

func w05_Add(t *testing.T, out *[]w05_Verdict, id, verdict, detail string) {
	t.Helper()
	*out = append(*out, w05_Verdict{id, verdict, detail})
	t.Logf("%s: %s - %s", id, verdict, detail)
	if verdict != "WORKS" {
		t.Errorf("%s %s: %s", id, verdict, detail)
	}
}

type w05_SORequest struct {
	body     []byte
	state    string
	question map[string]any
}

type w05_SO struct {
	srv     *httptest.Server
	posts   atomic.Int64
	fail    atomic.Bool
	hold    bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	reqs    []w05_SORequest
}

func (s *w05_SO) w05_unblock() { s.once.Do(func() { close(s.release) }) }

func (s *w05_SO) w05_last() w05_SORequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return w05_SORequest{}
	}
	return s.reqs[len(s.reqs)-1]
}

func w05_Label(q map[string]any) string {
	if q == nil {
		return "yes"
	}
	criteria, _ := q["criteria"].(map[string]any)
	if _, ok := criteria["yes"]; ok {
		return "yes"
	}
	keys := make([]string, 0, len(criteria))
	for k := range criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "yes"
	}
	return keys[0]
}

func w05_StartSO(t *testing.T, hold bool) *w05_SO {
	t.Helper()
	s := &w05_SO{hold: hold, entered: make(chan struct{}, 64), release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var in struct {
			State     string                     `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(raw, &in)
		var q map[string]any
		if b, ok := in.Questions["gate"]; ok {
			_ = json.Unmarshal(b, &q)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, w05_SORequest{body: append([]byte(nil), raw...), state: in.State, question: q})
		s.mu.Unlock()
		select {
		case s.entered <- struct{}{}:
		default:
		}
		if s.hold {
			select {
			case <-s.release:
			case <-time.After(4 * time.Second):
			}
		}
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"simulated SO failure"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"clef","answers":{"gate":{"type":"choice","choice":%q,"confidence":0.99}},"usage":{}}`, w05_Label(q))
	}))
	// Register unblock last: cleanups run LIFO, so a held stub is released
	// before httptest.Server.Close waits for its in-flight request.
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.w05_unblock)
	return s
}

func w05_Boot(t *testing.T, hold bool) (string, *w05_SO, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "w05_-sim-key")
	stub := w05_StartSO(t, hold)
	oldEP, oldClient := soEndpoint, soClient
	soEndpoint = stub.srv.URL + "/v1/systemone"
	soClient = stub.srv.Client()
	t.Cleanup(func() { soEndpoint, soClient = oldEP, oldClient })
	routerReset()
	t.Cleanup(routerReset)
	srv := httptest.NewServer(mcpHTTPHandler())
	return home, stub, srv
}

func w05_Caps(state, phase string) map[string]any {
	return map[string]any{
		"state": state, "phase": phase, "need_label": true, "no_scratch": true,
		"safe": true, "reversible": true, "authority": true,
		"calibrated": true, "opt_in": true,
	}
}

func w05_Frame(method string, params map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return b
}

func w05_Drain(s *w05_SO) {
	for {
		select {
		case <-s.entered:
		default:
			return
		}
	}
}

func w05_WaitSO(t *testing.T, s *w05_SO, want bool) bool {
	t.Helper()
	limit := 150 * time.Millisecond
	if want {
		limit = 2 * time.Second
	}
	select {
	case <-s.entered:
		return true
	case <-time.After(limit):
		return false
	}
}

func w05_Post(t *testing.T, srv *httptest.Server, frame []byte) (int, []byte, time.Duration) {
	t.Helper()
	client := srv.Client()
	client.Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST frame: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, body, time.Since(start)
}

func w05_ExpectSO(t *testing.T, srv *httptest.Server, stub *w05_SO, label string, frame []byte, wantSO bool) {
	t.Helper()
	w05_Drain(stub)
	before := stub.posts.Load()
	code, body, elapsed := w05_Post(t, srv, frame)
	if code != http.StatusOK || !bytes.Equal(body, frame) {
		t.Errorf("%s: HTTP %d body %q, want 200 and exact echo", label, code, body)
	}
	saw := w05_WaitSO(t, stub, wantSO)
	if saw != wantSO {
		t.Errorf("%s: SO call observed=%v want=%v (posts %d->%d, %v)",
			label, saw, wantSO, before, stub.posts.Load(), elapsed)
	}
}

func w05_RunBinary(t *testing.T, bin string, s *w05_SO, frame []byte) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-transport", "tools")
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT="+s.srv.URL+"/v1/systemone",
		"EVREN_API_KEY=w05_-binary-key",
	)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("binary stdin: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("binary stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shipped binary: %v", err)
	}
	if _, err := in.Write(frame); err != nil {
		t.Fatalf("write frame to binary: %v", err)
	}
	_ = in.Close()
	line, err := bufio.NewReader(out).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read binary reply: %v", err)
	}
	_ = cmd.Wait()
	return string(line)
}

func TestSimN2(t *testing.T) {
	home, stub, srv := w05_Boot(t, true)
	t.Cleanup(func() { stub.w05_unblock(); srv.Close() })
	var out []w05_Verdict

	cfg := filepath.Join(home, routerDir, "config.txt")
	if _, err := os.Lstat(cfg); !os.IsNotExist(err) {
		w05_Add(t, &out, "N2.1", "BROKEN", fmt.Sprintf("hermetic config.txt state: Lstat err=%v", err))
	} else if got := soMode(); got != soModeAdvisory {
		w05_Add(t, &out, "N2.1", "BROKEN", fmt.Sprintf("no config -> soMode()=%q want advisory", got))
	} else {
		w05_Add(t, &out, "N2.1", "WORKS", "no config.txt; soMode()=advisory; fail-open default")
	}

	frame := w05_Frame("initialize", w05_Caps("classify this n2 shadow request", "before-model"))
	client := srv.Client()
	client.Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("build N2 POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("N2 POST: %v", err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read flushed echo: %v", err)
	}
	delivered := time.Since(start)
	if delivered > 750*time.Millisecond {
		w05_Add(t, &out, "N2.2", "BROKEN", fmt.Sprintf("flushed body arrived in %v while SO was held; client observed delay", delivered))
	} else if !bytes.Equal(got, frame) {
		w05_Add(t, &out, "N2.2", "BROKEN", "flushed body differs from request frame")
	} else {
		w05_Add(t, &out, "N2.2", "WORKS", fmt.Sprintf("exact echo delivered in %v while SO stub was blocked", delivered))
	}
	if !w05_WaitSO(t, stub, true) {
		w05_Add(t, &out, "N2.3", "BROKEN", "no SO POST observed for in-scope advisory frame")
	}
	stub.w05_unblock()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if stub.posts.Load() != 1 {
		w05_Add(t, &out, "N2.3", "BROKEN", fmt.Sprintf("%d SO POSTs, want exactly 1", stub.posts.Load()))
	} else {
		raw, rerr := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
		if rerr != nil || !bytes.Contains(raw, []byte(`"reason":"so-admit"`)) {
			w05_Add(t, &out, "N2.3", "PARTIAL", fmt.Sprintf("SO called once but receipt.chain missing so-admit (err=%v)", rerr))
		} else {
			w05_Add(t, &out, "N2.3", "WORKS", "one shadow SO POST; chained so-admit receipt persisted")
		}
	}

	stub.fail.Store(true)
	w05_Drain(stub)
	failFrame := w05_Frame("initialize", w05_Caps("classify this n2 fail-open request", "before-model"))
	code, body, elapsed := w05_Post(t, srv, failFrame)
	if code != http.StatusOK || !bytes.Equal(body, failFrame) {
		w05_Add(t, &out, "N2.4", "BROKEN", fmt.Sprintf("SO 500 changed response: HTTP %d %q", code, body))
	} else if !w05_WaitSO(t, stub, true) || elapsed > 2*time.Second {
		w05_Add(t, &out, "N2.4", "BROKEN", fmt.Sprintf("fail-open latency %v or missing SO POST", elapsed))
	} else {
		w05_Add(t, &out, "N2.4", "WORKS", fmt.Sprintf("SO 500 -> HTTP 200 exact echo in %v", elapsed))
	}

	bin := simShippedBinary(t)
	if _, err := os.Stat(bin); err != nil {
		w05_Add(t, &out, "N2.5", "PARTIAL", fmt.Sprintf("shipped binary unavailable: %v", err))
	} else {
		stub.fail.Store(false)
		w05_Drain(stub)
		before := stub.posts.Load()
		binFrame := w05_Frame("initialize", w05_Caps("classify this binary n2 request", "before-model"))
		reply := w05_RunBinary(t, bin, stub, binFrame)
		saw := w05_WaitSO(t, stub, true)
		if !strings.Contains(reply, `"result"`) || !saw || stub.posts.Load() != before+1 {
			w05_Add(t, &out, "N2.5", "BROKEN", fmt.Sprintf("binary reply=%q SO posts %d->%d saw=%v", reply, before, stub.posts.Load(), saw))
		} else {
			w05_Add(t, &out, "N2.5", "WORKS", "shipped binary tools transport: reply plus one shadow SO POST")
		}
	}
}

func TestSimN3(t *testing.T) {
	_, stub, srv := w05_Boot(t, false)
	t.Cleanup(func() { stub.w05_unblock(); srv.Close() })
	var out []w05_Verdict

	type methodCase struct {
		label, method string
		inScope       bool
	}
	methods := []methodCase{
		{"initialize", "initialize", true},
		{"tools/call", "tools/call", true},
		{"notification", "notifications/initialized", false},
	}
	var scopeGaps []string
	for i, tc := range methods {
		p := w05_Caps(fmt.Sprintf("classify n3 method %d", i), "before-model")
		p["name"] = "sim.tool"
		w05_Drain(stub)
		before := stub.posts.Load()
		w05_ExpectSO(t, srv, stub, "N3.1 "+tc.label, w05_Frame(tc.method, p), tc.inScope)
		saw := stub.posts.Load() > before
		if saw != tc.inScope {
			scopeGaps = append(scopeGaps, fmt.Sprintf("%s(saw=%v,want=%v)", tc.label, saw, tc.inScope))
		}
	}
	if len(scopeGaps) != 0 {
		w05_Add(t, &out, "N3.1", "BROKEN", "documented two-method scope not reached: "+strings.Join(scopeGaps, " "))
	} else {
		w05_Add(t, &out, "N3.1", "WORKS", "two documented JSON-RPC methods call SO; notification does not")
	}

	phases := []struct {
		label   string
		phase   string
		inScope bool
	}{
		{"hyphen", "before-model", true},
		{"underscore", "before_model", true},
		{"uppercase", "BEFORE_MODEL", true},
		{"post-tool", "post-tool", false},
	}
	phaseGaps := []string{}
	for i, tc := range phases {
		p := w05_Caps(fmt.Sprintf("classify n3 phase %d", i), tc.phase)
		p["name"] = "sim.tool"
		w05_Drain(stub)
		before := stub.posts.Load()
		w05_ExpectSO(t, srv, stub, "N3.2 "+tc.label, w05_Frame("tools/call", p), tc.inScope)
		if (stub.posts.Load() > before) != tc.inScope {
			phaseGaps = append(phaseGaps, tc.label)
		}
	}
	if len(phaseGaps) != 0 {
		w05_Add(t, &out, "N3.2", "BROKEN", "phase normalization before gate failed for: "+strings.Join(phaseGaps, " "))
	} else {
		w05_Add(t, &out, "N3.2", "WORKS", "before-model/before_model/BEFORE_MODEL normalize to SO call; post-tool stays out")
	}

	capGaps := []string{}
	for _, flag := range []string{"opt_in", "authority", "calibrated"} {
		p := w05_Caps(fmt.Sprintf("classify n3 missing %s", flag), "before-model")
		p["name"] = "sim.tool"
		delete(p, flag)
		w05_Drain(stub)
		before := stub.posts.Load()
		w05_ExpectSO(t, srv, stub, "N3.3 no "+flag, w05_Frame("tools/call", p), false)
		if stub.posts.Load() > before {
			capGaps = append(capGaps, flag)
		}
	}
	if len(capGaps) != 0 {
		w05_Add(t, &out, "N3.3", "BROKEN", "missing transport capability still reached SO: "+strings.Join(capGaps, " "))
	} else {
		w05_Add(t, &out, "N3.3", "WORKS", "missing OptIn/Authority/Calibrated blocks the shadow SO call")
	}

	w05_Drain(stub)
	generic := w05_Caps("classify this unusual n3 request", "before-model")
	generic["name"] = "sim.tool"
	generic["item"] = "not.a.real.item"
	code, body, _ := w05_Post(t, srv, w05_Frame("tools/call", generic))
	saw := w05_WaitSO(t, stub, true)
	last := stub.w05_last()
	criteria, _ := last.question["criteria"].(map[string]any)
	if code != http.StatusOK || !saw || len(criteria) == 0 {
		w05_Add(t, &out, "N3.4", "BROKEN", fmt.Sprintf("unknown item/normal prose: HTTP %d saw=%v question=%v", code, saw, last.question))
	} else {
		keys := make([]string, 0, len(criteria))
		for k := range criteria {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		hasGeneric := true
		for _, want := range []string{"no", "other", "yes"} {
			found := false
			for _, k := range keys {
				if k == want {
					found = true
				}
			}
			if !found {
				hasGeneric = false
			}
		}
		if !hasGeneric || !bytes.Contains(body, []byte("unusual n3 request")) {
			w05_Add(t, &out, "N3.4", "BROKEN", fmt.Sprintf("fallback question labels=%v, echo=%q", keys, body))
		} else {
			var reg struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			_ = json.Unmarshal(decisionItemsJSON, &reg)
			named := false
			for _, it := range reg.Items {
				l := strings.ToLower(it.ID)
				if strings.Contains(l, "generic") || strings.Contains(l, "catch") || strings.Contains(l, "default") {
					named = true
				}
			}
			if named {
				w05_Add(t, &out, "N3.4", "WORKS", "unknown item falls back to generic yes/no/other; named catch-all present")
			} else {
				w05_Add(t, &out, "N3.4", "PARTIAL", "generic yes/no/other fallback works on the wire; no named catch-all item in 58-item registry")
			}
		}
	}

	var reg struct {
		Items []struct {
			ID          string `json:"id"`
			Requirement string `json:"requirement"`
		} `json:"items"`
	}
	_ = json.Unmarshal(decisionItemsJSON, &reg)
	var claims []string
	for _, it := range reg.Items {
		if strings.Contains(strings.ToLower(it.Requirement), "every request") {
			claims = append(claims, it.ID+": "+it.Requirement)
		}
	}
	if len(claims) != 0 {
		w05_Add(t, &out, "N3.5", "BROKEN", fmt.Sprintf("%d registry requirement strings still claim every request; first=%q", len(claims), claims[0]))
	} else {
		w05_Add(t, &out, "N3.5", "WORKS", "no every-request claim in served decision registry")
	}
}
