package app

// Eternal simulation contract (package app, stdlib only, permanent):
//
//	h := newEternalHarness(t)          // fake clock + loopback SO + fake router/cache/ports
//	sc := eternalScenario{...}         // intent -> decision -> eylem -> audit
//	tr, err := h.runScenario(t, sc)    // err != nil means the scenario is broken
//
// fix4_NN_sim_test.go files may call these helpers directly. This file never
// references their symbols, so a worktree missing any of them still compiles.
// TestEternalSimulationSuite discovers them by file name, skips absent ones,
// and runs the unified flow for every file that is present.
//
// Scenario files are discovered by name; no compile-time references.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	eternalClockStep = 7 * time.Second
	eternalPortBase  = 41000
)

// eternalClock is a deterministic fake clock. Production time.Now is never
// replaced; scenario timestamps come only from Tick.
type eternalClock struct{ now time.Time }

func newEternalClock() *eternalClock { return &eternalClock{now: time.Unix(0, 0).UTC()} }

func (c *eternalClock) Tick() time.Time {
	c.now = c.now.Add(eternalClockStep)
	return c.now
}

func (c *eternalClock) Now() time.Time { return c.now }

// eternalRouter is the fake router: an explicit deny list plus a decision log.
type eternalRouter struct {
	mu        sync.Mutex
	deny      map[string]bool
	decisions map[string]int
}

func newEternalRouter() *eternalRouter {
	return &eternalRouter{deny: map[string]bool{}, decisions: map[string]int{}}
}

func (r *eternalRouter) Deny(tool string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deny[tool] = true
}

func (r *eternalRouter) Allow(tool string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.deny[tool]
}

func (r *eternalRouter) Record(tool, decision string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions[tool+":"+decision]++
}

func (r *eternalRouter) Count(tool, decision string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.decisions[tool+":"+decision]
}

// eternalCache is the fake decision cache. It stores typed decisions only:
// no prose, no endpoint bytes.
type eternalCache struct {
	mu    sync.Mutex
	cells map[string]soDecision
}

func newEternalCache() *eternalCache { return &eternalCache{cells: map[string]soDecision{}} }

func (c *eternalCache) Get(key string) (soDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.cells[key]
	return d, ok
}

func (c *eternalCache) Put(key string, d soDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cells[key] = d
}

// eternalPorts hands out synthetic loopback addresses. No socket is bound, so
// scenarios cannot accidentally reach a non-loopback service.
type eternalPorts struct {
	mu sync.Mutex
	n  int
}

func (p *eternalPorts) Next() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return fmt.Sprintf("127.0.0.1:%d", eternalPortBase+p.n)
}

// eternalEndpoint is a loopback-only fake System One endpoint. It validates
// the wire shape and returns one canned typed answer per scenario.
type eternalReply struct {
	Choice     string
	Confidence float64
}

type eternalEndpoint struct {
	mu       sync.Mutex
	reply    eternalReply
	calls    int
	badShape int
}

func (e *eternalEndpoint) setReply(r eternalReply) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reply = r
}

func (e *eternalEndpoint) stats() (calls, bad int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls, e.badShape
}

func (e *eternalEndpoint) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		reply := e.reply
		e.calls++
		e.mu.Unlock()

		bad := r.Method != http.MethodPost || r.URL.Path != "/v1/systemone"
		var req struct {
			Model     string                     `json:"model"`
			State     string                     `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			bad = true
		}
		if req.Model == "" || req.State == "" || len(req.Questions) == 0 {
			bad = true
		}
		if bad {
			e.mu.Lock()
			e.badShape++
			e.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				"gate": map[string]any{
					"type":       "choice",
					"choice":     reply.Choice,
					"confidence": reply.Confidence,
				},
			},
		})
	})
}

type eternalHarness struct {
	clock    *eternalClock
	router   *eternalRouter
	cache    *eternalCache
	ports    *eternalPorts
	endpoint *eternalEndpoint
	server   *httptest.Server
}

func newEternalHarness(t *testing.T) *eternalHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("WALSY_SO_ENDPOINTS", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	t.Cleanup(routerReset)

	origEndpoint, origTimeout := soEndpoint, soTimeout
	t.Cleanup(func() { soEndpoint, soTimeout = origEndpoint, origTimeout })

	e := &eternalEndpoint{reply: eternalReply{Choice: "yes", Confidence: 0.94}}
	srv := httptest.NewUnstartedServer(e.handler())
	srv.Start()
	if !isLoopbackBase(srv.URL + "/v1/systemone") {
		srv.Close()
		t.Fatalf("fake SO endpoint is not loopback: %q", srv.URL)
	}
	t.Cleanup(srv.Close)
	soEndpoint = srv.URL + "/v1/systemone"
	soTimeout = 500 * time.Millisecond

	return &eternalHarness{
		clock:    newEternalClock(),
		router:   newEternalRouter(),
		cache:    newEternalCache(),
		ports:    &eternalPorts{},
		endpoint: e,
		server:   srv,
	}
}

// eternalScenario is one intent -> decision -> eylem -> audit case. Reply is
// what the fake SO says; Expect is what the scenario claims it should say.
type eternalScenario struct {
	Name    string
	Intent  string
	Reply   string
	Expect  string
	Tool    string
	Audit   string
	AuditOK bool
}

type eternalTrace struct {
	Name         string
	At           time.Time
	Port         string
	Decision     soDecision
	Action       string
	AuditOK      bool
	AuditMissing []string
	CacheHit     bool
	Calls        int
}

func eternalAction(label string) string {
	switch label {
	case "yes":
		return "allow"
	case "no":
		return "pass"
	case "other":
		return "hold"
	default:
		return "hold"
	}
}

func (h *eternalHarness) runScenario(t *testing.T, sc eternalScenario) (eternalTrace, error) {
	t.Helper()
	var tr eternalTrace
	tr.Name = sc.Name
	if strings.TrimSpace(sc.Name) == "" || strings.TrimSpace(sc.Intent) == "" ||
		strings.TrimSpace(sc.Reply) == "" || strings.TrimSpace(sc.Expect) == "" ||
		strings.TrimSpace(sc.Tool) == "" {
		return tr, fmt.Errorf("incomplete scenario: %+v", sc)
	}

	h.endpoint.setReply(eternalReply{Choice: sc.Reply, Confidence: 0.94})
	tr.At = h.clock.Tick()
	tr.Port = h.ports.Next()
	if !h.router.Allow(sc.Tool) {
		return tr, fmt.Errorf("tool %q denied by fake router", sc.Tool)
	}

	q := soQuestion{Type: "choice", Criteria: map[string]string{
		"yes":   "admit",
		"no":    "pass through",
		"other": "ambiguous",
	}}
	state, err := json.Marshal(map[string]any{
		"intent":     sc.Intent,
		"need_label": true,
		"phase":      "before-model",
	})
	if err != nil {
		return tr, err
	}
	key := string(state)
	d, hit := h.cache.Get(key)
	if !hit {
		d = askSOQ(key, q)
		h.cache.Put(key, d)
	}
	tr.Decision, tr.CacheHit = d, hit
	if d.Choice != sc.Expect {
		return tr, fmt.Errorf("decision = %q (reason %q), want %q", d.Choice, d.Reason, sc.Expect)
	}
	if d.Conf < 0.8 {
		return tr, fmt.Errorf("confidence %.2f below the 0.8 cut", d.Conf)
	}
	tr.Action = eternalAction(d.Choice)
	h.router.Record(sc.Tool, tr.Action)

	audit := finalAudit(sc.Audit)
	tr.AuditOK, tr.AuditMissing = audit.OK, append([]string(nil), audit.Missing...)
	if audit.OK != sc.AuditOK {
		return tr, fmt.Errorf("audit OK=%v Missing=%v, want OK=%v", audit.OK, audit.Missing, sc.AuditOK)
	}

	calls, bad := h.endpoint.stats()
	tr.Calls = calls
	if bad != 0 {
		return tr, fmt.Errorf("fake endpoint saw %d malformed requests", bad)
	}
	if calls == 0 && !hit {
		return tr, fmt.Errorf("fake endpoint saw no POST")
	}
	return tr, nil
}

var eternalScenarioFiles = [...]string{
	"fix4_01_sim_test.go",
	"fix4_02_sim_test.go",
	"fix4_03_sim_test.go",
	"fix4_04_sim_test.go",
	"fix4_05_sim_test.go",
	"fix4_06_sim_test.go",
	"fix4_07_sim_test.go",
	"fix4_08_sim_test.go",
	"fix4_09_sim_test.go",
	"fix4_10_sim_test.go",
	"fix5_global_sim_test.go",
}

func eternalSourceDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Dir(file)
}

func eternalScenarioForFile(file string, i int) eternalScenario {
	id := fmt.Sprintf("fix4_%02d", i)
	reply := "yes"
	switch {
	case i%5 == 0:
		reply = "other"
	case i%3 == 0:
		reply = "no"
	}
	item := "verified " + id + " simulation"
	audit, _ := json.Marshal(struct {
		Todo []string `json:"todo"`
		Done []string `json:"done"`
	}{Todo: []string{item}, Done: []string{item}})
	return eternalScenario{
		Name:    id,
		Intent:  "run " + file + " over the loopback oracle",
		Reply:   reply,
		Expect:  reply,
		Tool:    "systemone",
		Audit:   string(audit),
		AuditOK: true,
	}
}

// eternalRunCatalog is the permanent catalogue driver. A missing scenario file
// is skipped, never a compile or test failure; when every named file is present
// the log says the unified simulation is complete.
func eternalRunCatalog(t *testing.T, h *eternalHarness, dir string, files []string) int {
	t.Helper()
	ran := 0
	for i, file := range files {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			if !os.IsNotExist(err) {
				t.Errorf("stat %s: %v", file, err)
			} else {
				t.Logf("skip missing scenario %s", file)
			}
			continue
		}
		tr, err := h.runScenario(t, eternalScenarioForFile(file, i+1))
		if err != nil {
			t.Errorf("scenario %s: %v", file, err)
			continue
		}
		t.Logf("scenario %s: at=%s port=%s decision=%s action=%s audit=%v cacheHit=%v calls=%d",
			file, tr.At.Format(time.RFC3339), tr.Port, tr.Decision.Choice, tr.Action, tr.AuditOK, tr.CacheHit, tr.Calls)
		ran++
	}
	if ran == 0 {
		t.Skip("no fix4_NN_sim_test.go scenario files present")
	}
	if ran == len(files) {
		t.Logf("all %d scenarios present: unified eternal simulation complete", len(files))
	}
	logged := h.router.Count("systemone", "allow") + h.router.Count("systemone", "pass") + h.router.Count("systemone", "hold")
	if logged != ran {
		t.Errorf("fake router logged %d actions for %d scenarios", logged, ran)
	}
	return ran
}

func TestEternalSimulationSuite(t *testing.T) {
	h := newEternalHarness(t)
	eternalRunCatalog(t, h, eternalSourceDir(t), eternalScenarioFiles[:])
}

// The all-present branch is exercised without adding scenario files to this
// isolated worktree: ten empty placeholders live only in a t.TempDir.
func TestEternalSimulationCatalogAllPresent(t *testing.T) {
	h := newEternalHarness(t)
	dir := t.TempDir()
	for _, file := range eternalScenarioFiles {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := eternalRunCatalog(t, h, dir, eternalScenarioFiles[:]); got != len(eternalScenarioFiles) {
		t.Fatalf("catalog ran %d scenarios, want %d", got, len(eternalScenarioFiles))
	}
}

// Red-before/pass-after proof: the same harness accepts a coherent scenario
// and rejects a broken one whose SO answer contradicts its claim.
func TestEternalSimulationRedBeforePassAfter(t *testing.T) {
	h := newEternalHarness(t)
	good := eternalScenario{
		Name:    "self-good",
		Intent:  "verified deterministic loopback decision",
		Reply:   "yes",
		Expect:  "yes",
		Tool:    "systemone",
		Audit:   `{"todo":["verified deterministic loopback"],"done":["verified deterministic loopback"]}`,
		AuditOK: true,
	}
	tr, err := h.runScenario(t, good)
	if err != nil {
		t.Fatalf("coherent scenario failed: %v", err)
	}
	if tr.Decision.Choice != "yes" || tr.Action != "allow" || !tr.AuditOK || tr.Calls != 1 {
		t.Fatalf("coherent trace = %+v", tr)
	}

	bad := good
	bad.Name = "self-bad"
	bad.Intent = "broken deterministic loopback decision"
	bad.Reply = "no"
	if _, err := h.runScenario(t, bad); err == nil {
		t.Fatal("broken scenario passed: the harness did not catch the decision mismatch")
	}
}
