package app

// W19 simulation: exercise the real SO gate, stdio bridge, router, pin and
// receipt chain. All SO traffic goes to httptest stubs; HOME is temporary.

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const w19SOYes = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.96}}}`
const w19SONo = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"no","confidence":0.96}}}`
const w19ExplicitCaps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func w19StubSO(t *testing.T, calls *int32, reply func(n int32) (int, string)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		status, body := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func w19SwapEndpoint(t *testing.T, url string) {
	t.Helper()
	old := soEndpoint
	soEndpoint = url
	t.Cleanup(func() { soEndpoint = old })
}

func w19ScopeFrame(method string, id int, state string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + fmt.Sprint(id) + `,"method":"` + method +
		`","params":{"name":"read_file","state":"` + state + `",` + w19ExplicitCaps + `}}` + "\n")
}

func w19WriteSOConfig(t *testing.T, body string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func w19ReadAudit(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		t.Fatalf("audit read: %v", err)
	}
	return string(b)
}

// TestSimM2w19ScopeDependency: T2 makes F2/F3 unreachable. Only initialize and
// tools/call reach SO; a notification is a local pass with zero POSTs.
func TestSimM2w19ScopeDependency(t *testing.T) {
	var calls int32
	url := w19StubSO(t, &calls, func(int32) (int, string) { return 200, w19SOYes })
	w19SwapEndpoint(t, url)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()
	t.Cleanup(routerReset)

	d := newDecisionBridge()
	outOfScope := w19ScopeFrame("notifications/initialized", 1, "decide: trace")
	inScope := w19ScopeFrame("tools/call", 2, "decide: trace")

	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(append(append([]byte{}, outOfScope...), inScope...)), &toClient, &toServer, bytes.NewReader(nil))

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("SO POST count = %d, want 1 (scope is method-bound)", got)
	}
	if !bytes.Equal(toServer.Bytes(), append(append([]byte{}, outOfScope...), inScope...)) {
		t.Fatalf("advisory did not forward both frames: %q", toServer.Bytes())
	}
	if r := d.decideFrame(outOfScope); !r.Passthrough || r.Reason != "layer0-scope" {
		t.Fatalf("out-of-scope result = %+v, want layer0-scope pass", r)
	}
	if r := d.decideFrame(inScope); r.Reason != "cache-hit" && r.Reason != "so-admit" {
		t.Fatalf("in-scope result = %+v, want SO-backed decision", r)
	}
	audit := w19ReadAudit(t, home)
	if !strings.Contains(audit, "layer0-scope") || !strings.Contains(audit, "notifications/initialized") {
		t.Fatalf("audit missing local scope refusal: %s", audit)
	}
	if strings.Contains(audit, "NEVER") || strings.Contains(audit, "sk-") {
		t.Fatalf("audit leaked a fabricated id or key: %s", audit)
	}
}

// TestSimM2w19AdvisoryPlacementDependency: T3 makes N2 post-flush. The frame must
// reach upstream while the SO stub is still blocked.
func TestSimM2w19AdvisoryPlacementDependency(t *testing.T) {
	var calls int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, w19SOYes)
	}))
	t.Cleanup(srv.Close)
	w19SwapEndpoint(t, srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()
	t.Cleanup(routerReset)

	forwarded := make(chan []byte, 1)
	w := &w19NotifyWriter{ch: forwarded}
	frame := w19ScopeFrame("tools/call", 3, "decide: trace")
	done := make(chan struct{})
	go func() {
		defer close(done)
		bridgeStdio(bytes.NewReader(frame), io.Discard, w, bytes.NewReader(nil))
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("SO stub never entered: no decision attempted")
	}
	select {
	case got := <-forwarded:
		if !bytes.Equal(got, frame) {
			t.Fatalf("forwarded %q, want %q", got, frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frame NOT forwarded while SO blocked: pre-flush gate regression")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bridgeStdio did not drain advisory worker after SO release")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("SO POST count = %d, want 1", got)
	}
}

type w19NotifyWriter struct{ ch chan []byte }

func (w *w19NotifyWriter) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case w.ch <- b:
	default:
	}
	return len(p), nil
}

// TestSimM2w19SOBreakerDependency: T1 is only partly resolved. askSOQ performs
// exactly one POST per call (no retry), but the existing breaker still opens
// after three consecutive failures and suppresses the next POST.
func TestSimM2w19SOBreakerDependency(t *testing.T) {
	var calls int32
	status := int32(200)
	url := w19StubSO(t, &calls, func(int32) (int, string) {
		if atomic.LoadInt32(&status) == 500 {
			return 500, `not available`
		}
		return 200, w19SOYes
	})
	w19SwapEndpoint(t, url)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()
	t.Cleanup(routerReset)

	q := soQuestion{Type: "choice", Instructions: "pick", Criteria: map[string]string{"yes": "go", "no": "stop"}}
	atomic.StoreInt32(&status, 500)
	d1 := askSOQ("decide: trace", q)
	if d1.Choice != "" {
		t.Fatalf("500 response yielded a choice %q", d1.Choice)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("POSTs after one 500 = %d, want exactly 1 (no retry)", got)
	}
	atomic.StoreInt32(&status, 200)
	d2 := askSOQ("decide: trace", q)
	if d2.Choice != "yes" || d2.Conf < 0.96 {
		t.Fatalf("recovery call = %+v, want yes/0.96", d2)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("POSTs after recovery = %d, want 2", got)
	}
	atomic.StoreInt32(&status, 500)
	for i := 0; i < 3; i++ {
		_ = askSOQ("decide: trace", q)
	}
	before := atomic.LoadInt32(&calls)
	if !routerBreakerOpen("systemone") {
		t.Fatal("breaker did not open after three consecutive SO failures")
	}
	d4 := askSOQ("decide: trace", q)
	if d4.Choice != "" {
		t.Fatalf("open breaker still produced a choice %+v", d4)
	}
	if got := atomic.LoadInt32(&calls); got != before {
		t.Fatalf("open breaker sent %d extra POST(s), want 0", got-before)
	}
	if before != 5 {
		t.Fatalf("total POSTs before breaker = %d, want 5", before)
	}
}

// TestSimM2w19BuildDependency: N18 evidence is a byte-for-byte frame comparison
// between the in-process bridge and a fresh process running the same test
// binary. Health build info is logged; no digest is fabricated here.
func TestSimM2w19BuildDependency(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("EVREN_API_KEY", "")
	frames := append(w19ScopeFrame("tools/call", 41, "decide: trace"), w19ScopeFrame("tools/call", 42, "decide: trace")...)

	var localOut bytes.Buffer
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go w19EchoFrames(inR, outW)
	bridgeStdio(bytes.NewReader(frames), &localOut, inW, outR)
	_ = inW.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestSimM2w19HelperProcess$")
	cmd.Env = append(os.Environ(), "WALSY_W19_HELPER=1", "HOME="+os.Getenv("HOME"), "WALSY_SO_MODE=advisory", "EVREN_API_KEY=")
	cmd.Stdin = bytes.NewReader(frames)
	var childOut, childErr bytes.Buffer
	cmd.Stdout = &childOut
	cmd.Stderr = &childErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper process: %v; stderr: %s", err, childErr.String())
	}
	if !bytes.Equal(localOut.Bytes(), childOut.Bytes()) {
		t.Fatalf("frame-level equivalence broken:\nlocal %q\nchild %q", localOut.Bytes(), childOut.Bytes())
	}
	if !bytes.Contains(childOut.Bytes(), []byte(`"id":41`)) || !bytes.Contains(childOut.Bytes(), []byte(`"id":42`)) {
		t.Fatalf("child did not echo both frames: %q", childOut.Bytes())
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" || s.Key == "vcs.time" {
				t.Logf("build %s=%s", s.Key, s.Value)
			}
		}
	} else {
		t.Log("build info unavailable: no SHA proof from this process")
	}
}

// TestSimM2w19HelperProcess is the fresh-process side of the N18 simulation.
func TestSimM2w19HelperProcess(t *testing.T) {
	if os.Getenv("WALSY_W19_HELPER") != "1" {
		return
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go w19EchoFrames(inR, outW)
	bridgeStdio(os.Stdin, os.Stdout, inW, outR)
	_ = inW.Close()
	_ = outW.Close()
	os.Exit(0)
}

func w19EchoFrames(r io.Reader, w io.WriteCloser) {
	defer w.Close()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestSimM3w19RollbackSimulation: flip enforcing -> advisory via config (env
// cleared). A no-cap frame is blocked then forwarded; a drifted oracle pin is
// ignored in advisory. The receipt chain stays linked across both phases.
func TestSimM3w19RollbackSimulation(t *testing.T) {
	var calls int32
	url := w19StubSO(t, &calls, func(int32) (int, string) { return 200, w19SOYes })
	w19SwapEndpoint(t, url)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EVREN_API_KEY", "test-key")
	t.Setenv("WALSY_SO_MODE", "enforcing")
	w19WriteSOConfig(t, "so_mode=enforcing\n")
	routerReset()
	t.Cleanup(routerReset)

	// One bridge instance owns one receipt chain, as in a real process.
	d := newDecisionBridge()
	noCap := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"read_file"}}` + "\n")
	if out, blocked := d.enforcingGate(noCap); !blocked || !bytes.Contains(out, []byte(`"code":-32001`)) {
		t.Fatalf("enforcing did not block no-cap frame: blocked=%v out=%q", blocked, out)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("no-cap frames caused %d SO POST(s), want 0", got)
	}

	// Real admitted decisions produce receipts in enforcing mode.
	for _, id := range []int{12, 13} {
		capped := w19ScopeFrame("tools/call", id, fmt.Sprintf("decide: enforcing-%d", id))
		if out, blocked := d.enforcingGate(capped); blocked {
			t.Fatalf("enforcing blocked admitted frame %d: %q", id, out)
		}
	}

	// Simulated bad SO decision: pin identity drifts after bridge creation.
	t.Setenv("EVREN_API_KEY", "drifted-key")
	cappedDrift := w19ScopeFrame("tools/call", 14, "decide: drift")
	if out, blocked := d.enforcingGate(cappedDrift); !blocked || !bytes.Contains(out, []byte(`"code":-32001`)) {
		t.Fatalf("drifted oracle not blocked in enforcing: %q", out)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("drift check caused an SO POST: calls=%d, want 2", atomic.LoadInt32(&calls))
	}

	// Rollback: env cleared and config now advisory. The same no-cap frame is
	// forwarded; one capped frame keeps emitting receipts after rollback.
	t.Setenv("WALSY_SO_MODE", "")
	w19WriteSOConfig(t, "so_mode=advisory\n")
	if got := soMode(); got != soModeAdvisory {
		t.Fatalf("soMode after rollback = %q, want advisory", got)
	}
	if out, blocked := d.enforcingGate(noCap); blocked || len(out) != 0 {
		t.Fatalf("advisory rollback did not pass no-cap frame: blocked=%v out=%q", blocked, out)
	}
	routerBudgetReset() // new session after rollback; otherwise budget 3 is spent
	postFrame := w19ScopeFrame("tools/call", 15, "decide: advisory")
	if out, blocked := d.enforcingGate(postFrame); blocked || len(out) != 0 {
		t.Fatalf("post-rollback admitted frame blocked: blocked=%v out=%q", blocked, out)
	}

	w19VerifyReceiptChain(t, home)
}

// TestSimM3w19PinAndCacheRollback: rotate the pin and clear the cache after a bad
// SO answer; a stale admit must not survive either rollback. Receipt seq and
// hash chain must remain contiguous throughout.
func TestSimM3w19PinAndCacheRollback(t *testing.T) {
	answer := w19SOYes
	var calls int32
	url := w19StubSO(t, &calls, func(int32) (int, string) { return 200, answer })
	w19SwapEndpoint(t, url)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "test-key")
	routerReset()
	t.Cleanup(routerReset)

	d := newDecisionBridge()
	d.keySet = true
	d.pin.markSuccess(d.pin.current().Name) // stable generation, as in production after first success
	frame := w19ScopeFrame("tools/call", 12, "decide: trace")
	first := d.decideFrame(frame)
	if first.Decision != decPin || first.Reason != "so-admit" {
		t.Fatalf("first decision = %+v, want pin/so-admit", first)
	}
	second := d.decideFrame(frame)
	if second.Reason != "cache-hit" || second.Decision != decPin {
		t.Fatalf("second decision = %+v, want cache-hit", second)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("SO POSTs with cache = %d, want 1", got)
	}

	gen := d.pin.generation()
	d.pin.rotateOnFailure()
	if d.pin.generation() <= gen {
		t.Fatal("pin rotation did not bump generation")
	}
	answer = w19SONo
	afterRotate := d.decideFrame(frame)
	if afterRotate.Decision == decPin || afterRotate.Reason != "so-no" {
		t.Fatalf("decision after pin rotation = %+v, want fail-open so-no", afterRotate)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("SO POSTs after rotation = %d, want 2", got)
	}

	// Explicit cache clear removes the stale admit under the old generation.
	answer = w19SOYes
	d.cache = newShardedCache()
	afterClear := d.decideFrame(frame)
	if afterClear.Decision != decPin || afterClear.Reason != "so-admit" {
		t.Fatalf("decision after cache clear = %+v, want fresh so-admit", afterClear)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("SO POSTs after cache clear = %d, want 3", got)
	}
	w19VerifyReceiptChain(t, home)
}

func w19VerifyReceiptChain(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, routerDir, "receipt.chain")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("receipt chain read: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) < 3 {
		t.Fatalf("receipt lines = %d, want >=3 (rollback trace)", len(lines))
	}
	var prev []byte
	for i, line := range lines {
		var r receipt
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("receipt %d JSON: %v", i, err)
		}
		if r.ResponseID != "none" || r.Nonce == "" || r.Seq != i+1 || r.Prev != hex.EncodeToString(prev) {
			t.Fatalf("receipt %d broken linkage: %+v", i, r)
		}
		body, err := canonicalJSON(r)
		if err != nil {
			t.Fatal(err)
		}
		sum := chainHash(prev, body)
		prev = sum[:]
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("receipt file mode = %v, err %v, want 0600", fi, err)
	}
}

// TestSimM3w19ReceiptRestartChain is a strict RED simulation of a process
// restart (the rollback path). A fresh decisionBridge in the same HOME must
// continue the persisted chain, not start a new one.
func TestSimM3w19ReceiptRestartChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")

	firstBridge := newDecisionBridge()
	head1, err := firstBridge.chain.appendReceipt(receipt{Event: "decision", Decision: decPin, Reason: "so-admit", Method: "tools/call"})
	if err != nil {
		t.Fatal(err)
	}

	// Simulated restart: same HOME, new bridge instance.
	secondBridge := newDecisionBridge()
	head2, err := secondBridge.chain.appendReceipt(receipt{Event: "decision", Decision: decPass, Reason: "so-no", Method: "tools/call"})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(home, routerDir, "receipt.chain")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("receipt lines = %d, want 2", len(lines))
	}
	var first receipt
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatal(err)
	}
	firstBody, err := canonicalJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	sum1 := chainHash(nil, firstBody)
	if hex.EncodeToString(sum1[:]) != head1 {
		t.Fatalf("first head = %s, recomputed %x", head1, sum1)
	}
	var second receipt
	if err := json.Unmarshal(lines[1], &second); err != nil {
		t.Fatal(err)
	}
	if second.Seq != 2 || second.Prev != hex.EncodeToString(sum1[:]) {
		t.Fatalf("restart broke receipt chain: seq=%d prev=%q, want 2/%s", second.Seq, second.Prev, hex.EncodeToString(sum1[:]))
	}
	secondBody, err := canonicalJSON(second)
	if err != nil {
		t.Fatal(err)
	}
	sum2 := chainHash(sum1[:], secondBody)
	if hex.EncodeToString(sum2[:]) != head2 {
		t.Fatalf("second head = %s, recomputed %x", head2, sum2)
	}
}
