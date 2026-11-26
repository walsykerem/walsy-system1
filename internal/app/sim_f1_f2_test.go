package app

// Hermetic behavioural simulations for MANIFEST F1 and F2.
// No real SO call, no real toolchain, no process restart, no production edit.
// Every test drives real production code through stubs and observes real state.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const w01_caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func w01_env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	routerReset()
	t.Cleanup(routerReset)
	return home
}

func w01_so(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = orig; srv.Close() })
}

func w01_answer(choice string, conf float64) string {
	b, _ := json.Marshal(map[string]any{
		"model": "clef",
		"answers": map[string]any{"gate": map[string]any{
			"type": "choice", "choice": choice, "confidence": conf,
		}},
	})
	return string(b)
}

func w01_frame(state string) []byte {
	return []byte(`{"state":"` + state + `","method":"initialize",` + w01_caps + `}`)
}

// w01_buildImage replaces the toolchain with a stdlib-free shell stub. The real
// runGoBuild still constructs the command, so CGO_ENABLED and -buildvcs are
// observed, not assumed. The stub writes image bytes to the -o target.
func w01_buildImage(t *testing.T, home, src, rel, image string) (string, string) {
	t.Helper()
	bin := filepath.Join(home, "simbin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "sim-go.log")
	script := "#!/bin/sh\n" +
		"printf 'CGO_ENABLED=%s|GOOS=%s|GOARCH=%s|%s\\n' \"$CGO_ENABLED\" \"$GOOS\" \"$GOARCH\" \"$*\" >> \"$SIM_GO_LOG\"\n" +
		"out=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = '-o' ]; then out=\"$2\"; shift 2; else shift; fi\n" +
		"done\n" +
		"printf '%s' \"$SIM_IMAGE\" > \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SIM_IMAGE", image)
	t.Setenv("SIM_GO_LOG", logPath)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(src, rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := runGoBuild(context.Background(), src, rel, "darwin", "arm64"); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(src, rel))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), logPath
}

// ---- F1 ------------------------------------------------------------------

func TestSimF1BuildEnvAndInstallSwap(t *testing.T) {
	home := w01_env(t)
	src := filepath.Join(home, "src")
	digest, logPath := w01_buildImage(t, home, src, "dist/candidate", "SIM-NEW-IMAGE")

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := string(log)
	for _, want := range []string{"CGO_ENABLED=0|GOOS=darwin|GOARCH=arm64|", "-buildvcs=true", "-trimpath", "-o dist/candidate"} {
		if !strings.Contains(line, want) {
			t.Fatalf("build command missing %q: %s", want, line)
		}
	}
	wantSum := sha256.Sum256([]byte("SIM-NEW-IMAGE"))
	if digest != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("built image digest = %s, want %s", digest, hex.EncodeToString(wantSum[:]))
	}

	rel := filepath.Join("dist", "walsy-system1")
	if err := os.WriteFile(filepath.Join(src, rel), []byte("SIM-OLD-IMAGE"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installBuild(src, "dist/candidate", rel); err != nil {
		t.Fatalf("installBuild: %v", err)
	}
	gotNew, _ := os.ReadFile(filepath.Join(src, rel))
	gotPrev, _ := os.ReadFile(filepath.Join(src, rel+".prev"))
	if string(gotNew) != "SIM-NEW-IMAGE" || string(gotPrev) != "SIM-OLD-IMAGE" {
		t.Fatalf("swap wrong: new=%q prev=%q", gotNew, gotPrev)
	}
	if _, err := os.Stat(filepath.Join(src, "dist", "candidate")); !os.IsNotExist(err) {
		t.Fatalf("candidate not consumed by install: %v", err)
	}

	// Provenance: vcs_modified is a strict boolean of the actual build stamp.
	stamped := false
	revision := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.modified" {
				stamped = s.Value == "true"
			}
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
		}
	}
	h := healthFingerprint()
	v, ok := h["vcs_modified"].(bool)
	if !ok || v != stamped {
		t.Fatalf("vcs_modified = %v (%T), want bool %v", h["vcs_modified"], h["vcs_modified"], stamped)
	}
	if h["build_sha"] != revision {
		t.Fatalf("build_sha = %v, want stamped revision %q", h["build_sha"], revision)
	}
	if h["so_mode"] != soModeAdvisory {
		t.Fatalf("so_mode = %v, want advisory", h["so_mode"])
	}
}

func w01_serveFrames(t *testing.T, frames [][]byte) []byte {
	t.Helper()
	in := bytes.Join(frames, nil)
	var clientOut, upstream bytes.Buffer
	reply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")
	relayStdio(bytes.NewReader(in), &clientOut, &upstream, bytes.NewReader(reply))
	return upstream.Bytes()
}

func TestSimF1KeepAliveFrameEquivalence(t *testing.T) {
	home := w01_env(t)
	frames := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`),
		[]byte(`{"jsonrpc":"2.0","id":"u-1","method":"tools/call","params":{"name":"echo","arguments":{"text":"sim"}}}` + "\n"),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok"}}`),
	}
	// First frame needs its own newline; bytes.Join concatenates verbatim.
	frames[0] = append(frames[0], '\n')

	src := filepath.Join(home, "src")
	rel := filepath.Join("dist", "walsy-system1")
	old := []byte("SIM-OLD-IMAGE")
	if err := os.MkdirAll(filepath.Join(src, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, rel), old, 0755); err != nil {
		t.Fatal(err)
	}
	oldSum := sha256.Sum256(old)

	pidBefore := os.Getpid()
	before := w01_serveFrames(t, frames)
	if !bytes.Equal(before, bytes.Join(frames, nil)) {
		t.Fatalf("old image altered frames: got %q want %q", before, bytes.Join(frames, nil))
	}

	newDigest, _ := w01_buildImage(t, home, src, "dist/candidate", "SIM-NEW-IMAGE")
	if err := installBuild(src, "dist/candidate", rel); err != nil {
		t.Fatal(err)
	}
	if newDigest == hex.EncodeToString(oldSum[:]) {
		t.Fatal("installed image digest did not change")
	}
	after := w01_serveFrames(t, frames)
	pidAfter := os.Getpid()

	if pidBefore != pidAfter {
		t.Fatalf("serving identity changed: %d -> %d", pidBefore, pidAfter)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("frame-level equivalence broken after image swap:\nbefore %q\nafter  %q", before, after)
	}
	for i, raw := range frames {
		f, err := decodeFrame(raw)
		if err != nil || !bytes.Equal(f.Raw, raw) {
			t.Fatalf("frame %d not byte-preserved: err=%v raw=%q", i, err, raw)
		}
	}
}

// ---- F2 ------------------------------------------------------------------

// F2 requires operator config to stop supplying opt-ins the client omitted.
// On the current tree unwrapParams still backfills from config.txt, so this
// simulation fails and names the exact production gap.
func TestSimF2NoConfigOptInBackfill(t *testing.T) {
	home := w01_env(t)
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	cfg := "so_mode=enforcing\nneed_label=true\nno_scratch=true\nsafe=true\n" +
		"reversible=true\nauthority=true\ncalibrated=true\nopt_in=true\nphase=before-model\n"
	if err := writeStateFile(filepath.Join(home, ".walsy-system1", "config.txt"), []byte(cfg)); err != nil {
		t.Fatal(err)
	}
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`)
	d := newDecisionBridge()
	resp, blocked := d.enforcingGate(req)
	// Backfill removed; a frame with no opt-ins must fail closed even when
	// config.txt declares them. The control below proves config is not causal.
	wantBlocked := true
	if blocked != wantBlocked {
		t.Fatalf("F2 behaviour changed: blocked=%v resp=%s want=%v (re-check W01.md)", blocked, resp, wantBlocked)
	}
	t.Logf("F2 strict fail-closed: frame with no opt-ins blocked; resp=%s", resp)

	// Control: same frame, empty config, must block. Proves config is causal.
	cfgPath := filepath.Join(home, ".walsy-system1", "config.txt")
	if err := writeStateFile(cfgPath, nil); err != nil {
		t.Fatal(err)
	}
	if _, blocked := newDecisionBridge().enforcingGate(req); !blocked {
		t.Fatal("control: frame with no opt-ins and empty config was not blocked")
	}
}

func TestSimF2GateKeyBindingAndExpiry(t *testing.T) {
	w01_env(t)
	var calls atomic.Int64
	w01_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(w01_answer("yes", 0.9)))
	}))
	t.Setenv("EVREN_API_KEY", "sim-key-a")
	d := newDecisionBridge()
	d.pin.markSuccess(d.pin.current().Name) // pin first; a first admit pins mid-flight

	req := w01_frame("classify gate key case")
	if r := d.decide(req); r.Decision != decPin {
		t.Fatalf("first admit: %+v", r)
	}
	if r := d.decide(req); r.Decision != decPin || r.Reason != "cache-hit" {
		t.Fatalf("second call not a cache hit: %+v", r)
	}
	if calls.Load() != 1 {
		t.Fatalf("SO calls = %d, want 1", calls.Load())
	}

	cap := soCapability()
	base := []byte(`{"name":"read_file","arguments":{"path":"/tmp/sim-a"}}`)
	variant := []byte(`{"name":"read_file","arguments":{"path":"/tmp/sim-a","timestamp":9,"sessionId":"s9","msgRef":"m1"}}`)
	reordered := []byte(`{"arguments":{"path":"/tmp/sim-a"},"name":"read_file"}`)
	semantic := []byte(`{"name":"write_file","arguments":{"path":"/tmp/sim-a"}}`)
	if a, b := gateKey(base, cap), gateKey(variant, cap); a != b {
		t.Fatalf("volatile fields rebound gateKey: %s != %s", a, b)
	}
	if a, b := gateKey(base, cap), gateKey(reordered, cap); a != b {
		t.Fatalf("key order rebound gateKey: %s != %s", a, b)
	}
	if a, b := gateKey(base, cap), gateKey(semantic, cap); a == b {
		t.Fatal("semantic change did not rebind gateKey")
	}

	key := d.cacheKey(req, cap, d.pin.generation())
	if _, hit := d.cache.lookup(key); !hit {
		t.Fatal("admit cell not stored")
	}
	sh := d.cache.shard(key)
	sh.mu.Lock()
	el := sh.idx[hex.EncodeToString(key[:])]
	if el != nil {
		el.Value.(*cacheCell).expiresAt = time.Now().Add(-time.Second)
	}
	sh.mu.Unlock()
	if el == nil {
		t.Fatal("admit cell missing from shard index")
	}
	if _, hit := d.cache.lookup(key); hit {
		t.Fatal("expired cell still served")
	}
	if r := d.decide(req); r.Decision != decPin || calls.Load() != 2 {
		t.Fatalf("post-expiry call: %+v, SO calls=%d want 2", r, calls.Load())
	}

	if err := savePrompts([]Prompt{{Name: "sim", Text: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	k1 := gateKey(base, cap)
	if err := savePrompts([]Prompt{{Name: "sim", Text: "beta"}}); err != nil {
		t.Fatal(err)
	}
	if k2 := gateKey(base, cap); k1 == k2 {
		t.Fatal("prompt set change did not rebind gateKey")
	}

	keyA := d.cacheKey(req, cap, d.pin.generation())
	t.Setenv("EVREN_API_KEY", "sim-key-b")
	d2 := newDecisionBridge()
	keyB := d2.cacheKey(req, cap, d2.pin.generation())
	if keyA == keyB {
		t.Fatal("endpoint/key identity change did not rebind cache key")
	}
}

func TestSimF2EndpointKeyPinFailsClosed(t *testing.T) {
	w01_env(t)
	w01_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(w01_answer("yes", 0.9)))
	}))
	t.Setenv("EVREN_API_KEY", "sim-key-a")
	d := newDecisionBridge()
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},` + w01_caps + `}}`)

	t.Setenv("EVREN_API_KEY", "sim-key-b") // drift after session start
	t.Setenv("WALSY_SO_MODE", soModeEnforcing)
	resp, blocked := d.enforcingGate(req)
	if !blocked || !strings.Contains(string(resp), "so-unpinned-oracle") {
		t.Fatalf("drift not fail-closed: blocked=%v resp=%s", blocked, resp)
	}
	t.Setenv("WALSY_SO_MODE", "")
	if _, blocked := d.enforcingGate(req); blocked {
		t.Fatal("advisory mode consulted the pin and blocked a frame")
	}
}

func TestSimF2FailClosedAbstain(t *testing.T) {
	w01_env(t)
	var mode atomic.Int32
	w01_so(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			_, _ = w.Write([]byte(w01_answer("no", 0.9)))
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
		case 3:
			_, _ = w.Write([]byte(w01_answer("yes", 0.5)))
		default:
			_, _ = w.Write([]byte(w01_answer("yes", 0.9)))
		}
	}))
	t.Setenv("EVREN_API_KEY", "sim-key")
	d := newDecisionBridge()
	d.pin.markSuccess(d.pin.current().Name)

	mode.Store(1)
	r := d.decide(w01_frame("classify no case"))
	if !r.Abstained || r.Decision != decPass || r.Reason != "so-no" {
		t.Fatalf("terminal no must abstain: %+v", r)
	}
	if served := cachedResult(cachedDecision{Abstain: true, Reason: "so-no"}); served.Decision == decPin {
		t.Fatal("abstain cell served as admit")
	}
	if r2 := d.decide(w01_frame("classify no case")); !r2.Abstained || r2.Reason != "so-no" {
		t.Fatalf("cached abstain changed: %+v", r2)
	}

	mode.Store(2) // distinct state => distinct gate key
	r3 := d.decide(w01_frame("classify transient case"))
	if r3.Decision != decPass || r3.Reason != "so-degraded" {
		t.Fatalf("HTTP 500 must degrade: %+v", r3)
	}
	r4 := d.decide(w01_frame("classify transient case"))
	if r4.Decision != decPass || r4.Reason != "so-degraded" {
		t.Fatalf("degraded result frozen: %+v", r4)
	}

	mode.Store(3)
	r5 := d.decide(w01_frame("classify low conf case"))
	if !r5.Abstained || r5.Decision != decPass || r5.Reason != "so-low-conf" {
		t.Fatalf("low confidence must abstain: %+v", r5)
	}
}
