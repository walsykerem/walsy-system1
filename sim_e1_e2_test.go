package main

// W24 simulation: E1 enforcement transition and E2 stable-release criteria.
// All package-level helpers use the w24_ prefix. No production file is edited,
// no live SO endpoint is contacted, and no real key material is read.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

const (
	w24ShippedBinary = "./walsy-system1"
	w24ShippedSHA256 = "06ce7c6eda31186187e73e211b83240ede4106bebeb2d78f18563d3896c38af1"
	w24TestFile      = "sim_e1_e2_test.go"
	w24VerifyScript  = "scripts/verify.sh"
	w24Changelog     = "CHANGELOG.md"
)

type w24_SO struct {
	server     *httptest.Server
	mu         sync.Mutex
	bodies     []map[string]any
	choice     string
	conf       float64
	holdChan   chan struct{}
	entered    chan struct{}
	releaseOne sync.Once
	posts      atomic.Int64
}

type w24_Upstream struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	posts  atomic.Int64
}

func w24_Hermetic(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "w24-local-dummy-key")
	routerReset()
	t.Cleanup(routerReset)
	oldMetric := metricOut
	metricOut = io.Discard
	t.Cleanup(func() { metricOut = oldMetric })
	return home
}

func w24_WriteConfig(t *testing.T, home, text string) {
	t.Helper()
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func w24_NewSO(t *testing.T, choice string, conf float64) *w24_SO {
	t.Helper()
	s := &w24_SO{choice: choice, conf: conf}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	t.Cleanup(s.release)
	return s
}

func (s *w24_SO) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.posts.Add(1)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	choice, conf := s.choice, s.conf
	hold, entered := s.holdChan, s.entered
	s.mu.Unlock()
	if hold != nil {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-hold:
		case <-time.After(8 * time.Second):
		}
	}
	reply := map[string]any{
		"model": "clef",
		"answers": map[string]any{
			"gate": map[string]any{"type": "choice", "choice": choice, "confidence": conf},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (s *w24_SO) attach(t *testing.T) {
	t.Helper()
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint = s.server.URL
	soClient = s.server.Client()
	t.Setenv("WALSY_SO_ENDPOINT", s.server.URL)
	t.Cleanup(func() { soEndpoint, soClient = oldEndpoint, oldClient })
}

func (s *w24_SO) set(choice string, conf float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.choice, s.conf = choice, conf
}

func (s *w24_SO) holdSO(t *testing.T) (<-chan struct{}, <-chan struct{}) {
	t.Helper()
	s.mu.Lock()
	s.holdChan = make(chan struct{})
	s.entered = make(chan struct{}, 1)
	hold, entered := s.holdChan, s.entered
	s.mu.Unlock()
	return hold, entered
}

func (s *w24_SO) release() {
	s.releaseOne.Do(func() {
		s.mu.Lock()
		hold := s.holdChan
		s.mu.Unlock()
		if hold != nil {
			close(hold)
		}
	})
}

func (s *w24_SO) count() int { return int(s.posts.Load()) }

func (s *w24_SO) bodiesCopy() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.bodies))
	copy(out, s.bodies)
	return out
}

func w24_NewUpstream(t *testing.T) *w24_Upstream {
	t.Helper()
	u := &w24_Upstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		u.posts.Add(1)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *w24_Upstream) count() int { return int(u.posts.Load()) }

func (u *w24_Upstream) last() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return u.bodies[len(u.bodies)-1]
}

func w24_Frame(id, state string, caps bool) []byte {
	params := map[string]any{
		"state":      state,
		"item":       "intake.scope",
		"name":       "system1_decide",
		"phase":      "before-model",
		"need_label": true,
		"no_scratch": true,
		"safe":       true,
		"reversible": true,
		"authority":  true,
		"calibrated": true,
		"opt_in":     caps,
	}
	env := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params}
	b, _ := json.Marshal(env)
	return b
}

func w24_Post(h http.Handler, body []byte) (int, []byte) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func w24_RPCError(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var p struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("response is not JSON-RPC: %v: %s", err, body)
	}
	return p.Error.Code, p.Error.Message
}

func w24_RequireForward(t *testing.T, code int, got, want []byte, before, after int) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("forward status = %d, want 200; body=%s", code, got)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("upstream response diverged: got %s want %s", got, want)
	}
	if after != before+1 {
		t.Fatalf("upstream posts = %d, want %d (before %d)", after, before+1, before)
	}
}

func w24_RequireBlocked(t *testing.T, code int, body []byte, before, after int, needle string) {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("block status = %d, want JSON-RPC 200; body=%s", code, body)
	}
	c, msg := w24_RPCError(t, body)
	if c != -32001 {
		t.Fatalf("block error code = %d, want -32001; body=%s", c, body)
	}
	if !strings.Contains(msg, needle) {
		t.Fatalf("block message %q does not contain %q", msg, needle)
	}
	if after != before {
		t.Fatalf("blocked frame reached upstream: posts %d -> %d", before, after)
	}
}

func w24_Questions(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		t.Fatal("SO POST body was not decoded")
	}
	q, ok := body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("SO POST has no questions object: %v", body)
	}
	return q
}

// TestSimE1W24TransportEnforcement exercises the real HTTP decision gate.
func TestSimE1W24TransportEnforcement(t *testing.T) {
	home := w24_Hermetic(t)
	// Enforcing promotion is fail-closed: declare both preconditions on the
	// real config so the rest of this test still exercises the capability gate.
	w24_WriteConfig(t, home, "m1_approval_owner=researcher\nso_go_confidence=0.9\n")
	so := w24_NewSO(t, "inside", 0.95)
	so.attach(t)
	up := w24_NewUpstream(t)

	advisory := mcpHTTPHandlerUpstream(up.server.URL)
	frame := w24_Frame("w24-adv", "classify scope for W24 advisory simulation", true)
	code, out := w24_Post(advisory, frame)
	w24_RequireForward(t, code, out, frame, 0, up.count())
	if !bytes.Equal(up.last(), frame) {
		t.Fatalf("upstream did not receive the exact client frame")
	}
	if so.count() != 1 {
		t.Fatalf("advisory SO posts = %d, want 1", so.count())
	}
	q := w24_Questions(t, so.bodiesCopy()[0])
	if _, ok := q["gate"]; !ok {
		t.Fatalf("single decision question must be keyed gate: %v", q)
	}
	for _, forbidden := range []string{"go", "final", "complete", "m1", "approval"} {
		if _, ok := q[forbidden]; ok {
			t.Fatalf("unexpected E1 precondition question %q in SO payload: %v", forbidden, q)
		}
	}

	t.Setenv("WALSY_SO_MODE", "enforcing")
	enforcing := mcpHTTPHandlerUpstream(up.server.URL)

	beforeSO, beforeUp := so.count(), up.count()
	noCaps := w24_Frame("w24-nocap", "classify scope for W24 cap-missing simulation", false)
	code, out = w24_Post(enforcing, noCaps)
	w24_RequireBlocked(t, code, out, beforeUp, up.count(), "explicit decision capability")
	if so.count() != beforeSO {
		t.Fatalf("cap-missing block still called SO: %d -> %d", beforeSO, so.count())
	}

	so.set("inside", 0.95)
	beforeUp = up.count()
	admit := w24_Frame("w24-admit", "classify scope for W24 admit simulation", true)
	code, out = w24_Post(enforcing, admit)
	w24_RequireForward(t, code, out, admit, beforeUp, up.count())

	so.set("outside", 0.95)
	beforeUp = up.count()
	deny := w24_Frame("w24-deny", "classify scope for W24 nonproceed simulation", true)
	code, out = w24_Post(enforcing, deny)
	w24_RequireBlocked(t, code, out, beforeUp, up.count(), "BLOCK and ask the human")

	so.set("inside", 0.79)
	beforeUp = up.count()
	low := w24_Frame("w24-lowconf", "classify scope for W24 low-confidence simulation", true)
	code, out = w24_Post(enforcing, low)
	w24_RequireForward(t, code, out, low, beforeUp, up.count())
}

type w24_HTTPResult struct {
	code int
	body []byte
	err  error
}

// TestSimE1W24T3AndRollback proves post-flush advisory timing and mode rollback.
func TestSimE1W24T3AndRollback(t *testing.T) {
	home := w24_Hermetic(t)
	w24_WriteConfig(t, home, "m1_approval_owner=researcher\nso_go_confidence=0.9\n")
	so := w24_NewSO(t, "inside", 0.95)
	so.attach(t)
	up := w24_NewUpstream(t)
	hold, entered := so.holdSO(t)

	handler := mcpHTTPHandlerUpstream(up.server.URL)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	frame := w24_Frame("w24-t3", "classify scope for W24 post-flush simulation", true)
	res := make(chan w24_HTTPResult, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(srv.URL+"/mcp", "application/json", bytes.NewReader(frame))
		if err != nil {
			res <- w24_HTTPResult{err: err}
			return
		}
		defer resp.Body.Close()
		// Read exactly the forwarded frame, not to EOF: the final chunk is
		// written when the handler returns, after the post-flush SO call.
		body := make([]byte, len(frame))
		_, err = io.ReadFull(resp.Body, body)
		res <- w24_HTTPResult{code: resp.StatusCode, body: body, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("advisory SO was never called; T3 path not exercised")
	}
	select {
	case got := <-res:
		if got.err != nil {
			t.Fatalf("client failed while SO was held: %v", got.err)
		}
		w24_RequireForward(t, got.code, got.body, frame, 0, up.count())
	case <-time.After(3 * time.Second):
		t.Fatal("client response waited for the held SO call; decision runs before flush")
	}
	select {
	case <-hold:
		t.Fatal("SO hold was released before the client response check")
	default:
	}
	so.release()

	t.Setenv("WALSY_SO_MODE", "enforcing")
	blockHandler := mcpHTTPHandlerUpstream(up.server.URL)
	before := up.count()
	noCaps := w24_Frame("w24-rollback-block", "classify scope for W24 rollback block", false)
	code, out := w24_Post(blockHandler, noCaps)
	w24_RequireBlocked(t, code, out, before, up.count(), "explicit decision capability")

	t.Setenv("WALSY_SO_MODE", "advisory")
	forwardHandler := mcpHTTPHandlerUpstream(up.server.URL)
	before = up.count()
	rollback := w24_Frame("w24-rollback-pass", "classify scope for W24 rollback pass", false)
	code, out = w24_Post(forwardHandler, rollback)
	w24_RequireForward(t, code, out, rollback, before, up.count())
}

// TestSimE1W24ApprovalPreconditionsMissing is a strict red test for the E1
// promotion contract in MANIFEST: SO go >= 0.8 and M1 approver permission.
func TestSimE1W24ApprovalPreconditionsMissing(t *testing.T) {
	home := w24_Hermetic(t)
	w24_WriteConfig(t, home, "so_mode=enforcing\n")
	so := w24_NewSO(t, "inside", 0.95)
	so.attach(t)
	up := w24_NewUpstream(t)

	before := up.count()
	frame := w24_Frame("w24-approval", "classify scope for W24 approval simulation", true)
	code, out := w24_Post(mcpHTTPHandlerUpstream(up.server.URL), frame)
	bodies := so.bodiesCopy()
	if len(bodies) == 0 {
		t.Fatalf("BROKEN E1: no SO question reached the stub (code=%d, out=%s); expected go/approval questions", code, out)
	}
	if code == http.StatusOK && up.count() == before+1 && !bytes.Contains(out, []byte("-32001")) {
		t.Errorf("BROKEN E1: enforcing accepted with no M1 approver and no SO-go >= 0.8 question; SO questions=%v", w24_Questions(t, bodies[0]))
	}
	q := w24_Questions(t, bodies[0])
	foundGo, foundApproval := false, false
	for key := range q {
		if strings.EqualFold(key, "go") || strings.Contains(strings.ToLower(key), "complete") {
			foundGo = true
		}
		if strings.Contains(strings.ToLower(key), "approval") || strings.Contains(strings.ToLower(key), "m1") {
			foundApproval = true
		}
	}
	if !foundGo || !foundApproval {
		t.Errorf("BROKEN E1: SO payload lacks go-confidence (%v) or M1 approval (%v) question: %v", foundGo, foundApproval, q)
	}
}

func w24_SHA256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func w24_BuildInfo(t *testing.T, path string) map[string]string {
	t.Helper()
	out, err := exec.Command("go", "version", "-m", path).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s: %v\n%s", path, err, out)
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "build" {
			kv := strings.SplitN(f[1], "=", 2)
			if len(kv) == 2 {
				m[kv[0]] = kv[1]
			} else if len(f) >= 3 {
				m[f[1]] = strings.Join(f[2:], " ")
			} else {
				m[f[1]] = "true"
			}
		}
		if len(f) >= 2 && f[0] == "mod" {
			m["mod"] = strings.Join(f[1:], " ")
		}
	}
	return m
}

// TestSimE2W24ArtifactFingerprint checks the shipped artifact and a fresh
// temp-dir build without writing to the repository.
func TestSimE2W24ArtifactFingerprint(t *testing.T) {
	digest := w24_SHA256File(t, w24ShippedBinary)
	// The checked-in literal describes the darwin build. Linux CI pins the
	// native build hash in a digest fixture instead of baking a second literal.
	want := w24ShippedSHA256
	if p := os.Getenv("WALSY_SIM_DIGEST"); p != "" {
		_, pinned := simReadDigest(t, p)
		want = pinned.Target.FileSHA256
	}
	if digest != want {
		t.Fatalf("shipped binary digest = %s, want %s", digest, want)
	}
	shipped := w24_BuildInfo(t, w24ShippedBinary)
	if shipped["CGO_ENABLED"] != "0" || shipped["-trimpath"] != "true" {
		t.Fatalf("shipped build flags = CGO_ENABLED:%q trimpath:%q, want 0/true", shipped["CGO_ENABLED"], shipped["-trimpath"])
	}
	if shipped["vcs.revision"] != "" || shipped["vcs.modified"] != "" {
		t.Fatalf("shipped build carries git state: revision=%q modified=%q; want deterministic -buildvcs=false", shipped["vcs.revision"], shipped["vcs.modified"])
	}
	if !strings.Contains(shipped["mod"], "walsy-system1") {
		t.Fatalf("shipped module stamp = %q", shipped["mod"])
	}

	wantItems := sha256.Sum256(decisionItemsJSON)
	fp := healthFingerprint()
	if fp["ok"] != true {
		t.Fatalf("healthFingerprint ok = %v, want true", fp["ok"])
	}
	if got, ok := fp["items_sha256"].(string); !ok || got != hex.EncodeToString(wantItems[:]) {
		t.Fatalf("items_sha256 = %v, want %s", fp["items_sha256"], hex.EncodeToString(wantItems[:]))
	}
	if got, ok := fp["vcs_modified"].(bool); !ok {
		t.Fatalf("vcs_modified has type %T, want bool", fp["vcs_modified"])
	} else if !got {
		t.Log("PARTIAL E2: test binary reports vcs_modified=false; source tree is dirty but test build is unstamped")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpBin := filepath.Join(t.TempDir(), "walsy-system1")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", tmpBin, ".")
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rebuild failed: %v\n%s", err, out)
	}
	rebuilt := w24_BuildInfo(t, tmpBin)
	if rebuilt["CGO_ENABLED"] != "0" || rebuilt["-trimpath"] != "true" {
		t.Fatalf("rebuilt flags = CGO_ENABLED:%q trimpath:%q, want 0/true", rebuilt["CGO_ENABLED"], rebuilt["-trimpath"])
	}
	if rebuilt["vcs.revision"] != "" || rebuilt["vcs.modified"] != "" {
		t.Fatalf("rebuilt build carries git state: revision=%q modified=%q; want deterministic -buildvcs=false", rebuilt["vcs.revision"], rebuilt["vcs.modified"])
	}
	rebuiltDigest := w24_SHA256File(t, tmpBin)
	if rebuiltDigest != digest {
		t.Fatalf("rebuilt binary digest = %s, want shipped %s", rebuiltDigest, digest)
	}
	t.Log("E2 F1: both builds are vcs-unstamped and digest-equal; dirty/clean tree cannot change fixture")
}

// TestSimE2W24VerifyShape simulates the read-only parts of scripts/verify.sh.
// The full script is not run: step 4 needs CGO=1 and step 3 rewrites the
// repository binary, which this simulation must not do.
func TestSimE2W24VerifyShape(t *testing.T) {
	raw, err := os.ReadFile(w24VerifyScript)
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for _, must := range []string{
		"CGO_ENABLED=0 go vet ./...",
		"CGO_ENABLED=1 go test -race -count=1 ./...",
		"CGO_ENABLED=0 go build -trimpath -buildvcs=false -o walsy-system1 .",
		"go run ./scripts/recondigest",
		"echo \"PASS\"",
	} {
		if !strings.Contains(script, must) {
			t.Fatalf("verify.sh missing required step %q", must)
		}
	}
	buildLine := "CGO_ENABLED=0 go build -trimpath -buildvcs=false -o walsy-system1 ."
	testLine := "CGO_ENABLED=1 go test -race -count=1 ./..."
	if strings.Index(script, buildLine) >= strings.Index(script, testLine) {
		t.Fatalf("verify.sh must build the shipped binary before -race tests read it")
	}
	cmd := exec.Command("gofmt", "-l", w24TestFile)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gofmt -l %s: %v\n%s", w24TestFile, err, out)
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("gofmt needed for %s: %s", w24TestFile, out)
	}
}

// TestSimE2W24FinalCompleteGateMissing is a strict red test for the stable
// release criterion "SO final complete >= 0.8".
func TestSimE2W24FinalCompleteGateMissing(t *testing.T) {
	w24_Hermetic(t)
	so := w24_NewSO(t, "inside", 0.95)
	so.attach(t)
	up := w24_NewUpstream(t)
	frame := w24_Frame("w24-final", "classify scope for W24 final-complete simulation", true)
	code, out := w24_Post(mcpHTTPHandlerUpstream(up.server.URL), frame)
	w24_RequireForward(t, code, out, frame, 0, up.count())
	q := w24_Questions(t, so.bodiesCopy()[0])
	if _, ok := q["gate"]; !ok {
		t.Fatalf("expected the ordinary gate question: %v", q)
	}
	for key := range q {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "final") || strings.Contains(lower, "complete") {
			return
		}
	}
	t.Errorf("BROKEN E2: no SO final/complete >= 0.8 question is issued before forwarding: %v", q)
}

// TestSimE2W24ReleaseAnnouncementMissing is a strict red test for a stable
// release announcement that names the M1 approval owner.
func TestSimE2W24ReleaseAnnouncementMissing(t *testing.T) {
	raw, err := os.ReadFile(w24Changelog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "1.0.0-fix-7") {
		t.Fatalf("CHANGELOG does not contain the claimed 1.0.0-fix-7 release")
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "m1") || !strings.Contains(lower, "approv") {
		t.Errorf("BROKEN E2: stable release announcement has no M1 approval owner: %s", w24Changelog)
	}
}
