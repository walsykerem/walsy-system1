package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAskSOFailurePassthrough(t *testing.T) {
	routerReset()
	t.Cleanup(routerReset)
	t.Setenv("EVREN_API_KEY", "test-key")
	orig := soEndpoint
	defer func() { soEndpoint = orig }()

	// HTTP 500 -> no decision, never an error surfaced to caller.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	soEndpoint = srv.URL
	if d := askSO("x"); d.Choice != "" || d.Conf != 0 {
		t.Fatalf("500 should be empty decision, got %+v", d)
	}
	srv.Close()

	// Low confidence -> degraded passthrough.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.79}}}`))
	}))
	soEndpoint = srv2.URL
	if d := askSO("x"); d.Choice != "yes" || d.Conf != 0.79 {
		t.Fatalf("parser keeps the below-cut label for the caller to steer, got %+v", d)
	}
	srv2.Close()

	// Out-of-range confidence (negative) -> empty decision, never admitted.
	srvNeg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":-0.5}}}`))
	}))
	soEndpoint = srvNeg.URL
	if d := askSO("x"); d.Choice != "" || d.Conf != 0 {
		t.Fatalf("negative confidence must be rejected, got %+v", d)
	}
	srvNeg.Close()

	// Timeout -> TimedOut set, still empty decision.
	origTO := soTimeout
	soTimeout = 20 * time.Millisecond
	defer func() { soTimeout = origTO }()
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	soEndpoint = srv3.URL
	if d := askSO("x"); !d.TimedOut || d.Choice != "" {
		t.Fatalf("timeout should be TimedOut+empty, got %+v", d)
	}
	srv3.Close()
}

// TestAskSOStoredProviderKey proves the SO key comes from any registry provider
// whose base matches the SO endpoint, not only from one hard-coded env name.
func TestAskSOStoredProviderKey(t *testing.T) {
	routerReset()
	t.Cleanup(routerReset)
	const base = "https://local-so.example/v1/systemone"
	orig := soEndpoint
	soEndpoint = base
	defer func() { soEndpoint = orig }()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EVREN_API_KEY", "")
	state := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	writeProviders(t, state, "LOCAL-SO,"+base+",LOCAL_KEY\n", 0600)
	if err := saveKey("LOCAL-SO", "stored-key"); err != nil {
		t.Fatal(err)
	}

	cap := &headerCapture{body: `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`}
	setCaptureClient(t, cap)
	d := askSO("state")
	if d.Choice != "yes" || d.Conf < 0.95 {
		t.Fatalf("stored provider key not used: choice=%q conf=%v", d.Choice, d.Conf)
	}
	if got := cap.header.Get("x-api-key"); got != "stored-key" {
		t.Fatalf("stored provider x-api-key = %q, want stored-key", got)
	}
}

// headerCapture answers SO in-process so a non-loopback-shaped base needs no
// real network; only request headers are retained for assertions.
type headerCapture struct {
	header http.Header
	body   string
}

func (c *headerCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	c.header = req.Header.Clone()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    req,
	}, nil
}

func setCaptureClient(t *testing.T, c *headerCapture) {
	t.Helper()
	orig := soClient
	soClient = &http.Client{Transport: c}
	t.Cleanup(func() { soClient = orig })
}

// J1: a loopback judge gets no key; a non-loopback-shaped base still gets the
// exact same key, intercepted in-process.
func TestSOKeyOnlyForRemoteBase(t *testing.T) {
	const key = "j1-test-key"
	t.Setenv("EVREN_API_KEY", key)
	routerReset()
	t.Cleanup(routerReset)
	origEndpoint, origClient := soEndpoint, soClient
	t.Cleanup(func() { soEndpoint, soClient = origEndpoint, origClient })

	var localKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localKey = r.Header.Get("x-api-key")
		_, _ = io.WriteString(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
	}))
	defer srv.Close()
	soEndpoint = srv.URL
	if d := askSO("local judge"); d.Choice != "yes" {
		t.Fatalf("loopback choice=%q reason=%q", d.Choice, d.Reason)
	}
	if localKey != "" {
		t.Fatalf("loopback received x-api-key %q", localKey)
	}

	cap := &headerCapture{body: `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`}
	setCaptureClient(t, cap)
	soEndpoint = "https://remote-judge.example/v1/systemone"
	if d := askSO("remote judge"); d.Choice != "yes" {
		t.Fatalf("remote choice=%q reason=%q", d.Choice, d.Reason)
	}
	if got := cap.header.Get("x-api-key"); got != key {
		t.Fatalf("remote x-api-key = %q, want %q", got, key)
	}
}

// F1: optional SO headers land only in the audit allowlist. No state, key,
// confidence or server-time reaches disk, a receipt or a log.
func TestSOResponseIDAuditOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	routerReset()
	t.Cleanup(routerReset)
	orig := soEndpoint
	t.Cleanup(func() { soEndpoint = orig })

	auditPath := filepath.Join(home, routerDir, routerAuditName)
	responseIDs := func() []string {
		t.Helper()
		data, err := os.ReadFile(auditPath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatal(err)
			}
			if v, ok := m["response_id"]; ok {
				out = append(out, v)
			}
		}
		return out
	}

	const state = "secret-f1-state-must-stay-off-disk"
	t.Setenv("EVREN_API_KEY", "secret-f1-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-typesafe-request-id", "req-f1-7f3a91")
		w.Header().Set("x-flide-server-time-ms", "42")
		_, _ = io.WriteString(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
	}))
	soEndpoint = srv.URL
	d := askSO(state)
	srv.Close()
	if d.Choice != "yes" {
		t.Fatalf("choice=%q reason=%q", d.Choice, d.Reason)
	}
	ids := responseIDs()
	if len(ids) != 1 || ids[0] != "req-f1-7f3a91" {
		t.Fatalf("response_ids = %v, want [req-f1-7f3a91]", ids)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{state, "secret-f1-key", "confidence", "server_time", "x-flide-server-time-ms"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("audit leaked %q", leak)
		}
	}

	// No header: no new response_id record.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
	}))
	soEndpoint = srv2.URL
	d = askSO(state)
	srv2.Close()
	if d.Choice != "yes" {
		t.Fatalf("choice=%q reason=%q", d.Choice, d.Reason)
	}
	if got := responseIDs(); len(got) != len(ids) {
		t.Fatalf("no-header response_ids = %v, want unchanged", got)
	}
}

// J1: a keyless loopback provider reaches the pool; a keyless remote one does not.
func TestSOPoolKeylessLoopbackCandidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("EVREN_API_KEY", "")
	orig := soEndpoint
	soEndpoint = soEndpointDefaultBase
	t.Cleanup(func() { soEndpoint = orig })

	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	// Baked slots: Flide Local is keyless + loopback, Ollama Cloud is keyless
	// + remote. No providers.txt, so the baked registry is in force.
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"),
		[]byte("Flide Local\tmodel-flide\nOllama Cloud\tmodel-remote\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var flide *soCandidate
	for _, c := range soPool() {
		if c.Name == "Ollama Cloud" {
			t.Fatal("keyless non-loopback candidate must stay out of the pool")
		}
		if c.Name == "Flide Local" {
			cp := c
			flide = &cp
		}
	}
	if flide == nil || flide.Key != "" || !isLoopbackBase(flide.Base) {
		t.Fatalf("keyless loopback candidate missing from pool: %+v", flide)
	}
}
