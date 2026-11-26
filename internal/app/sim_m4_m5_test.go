package app

// W20 hermetic simulation for MANIFEST M4 (evidence storage path convention)
// and M5 (per-item who/when/done). Real stdio transport, real dashboard
// handler, real decision engine and the shipped binary run against an
// httptest SO stub and a temporary HOME. A red check is evidence of a real
// missing behaviour, not a broken harness.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// Every package-level identifier below is w20_-prefixed. The two TestSimM4/
// TestSimM5 entry points keep their mandated names so -run selects them.
type w20_check struct {
	id      string
	verdict string
	detail  string
}

// w20_key is a synthetic redaction sentinel, not a credential: no service accepts it.
const w20_key = "w20-sim-key-9f3a7c1d"
const w20_sentinel = "w20-redaction-sentinel-7c1d"
const w20_answer = `{"model":"clef","answers":{"gate":{"type":"choice","choice":"yes","confidence":0.96}},"usage":{}}`

// Same arch map CI sets: a repo-relative fixture, so a local
// run without WALSY_SIM_ARCHMAP judges the same evidence CI does.
const w20_default_arch = "testdata/ci-linux"

func w20_add(t *testing.T, out *[]w20_check, id, verdict, detail string) {
	t.Helper()
	*out = append(*out, w20_check{id, verdict, detail})
	t.Logf("%s: %s - %s", id, verdict, detail)
	if verdict != "WORKS" {
		t.Errorf("%s %s: %s", id, verdict, detail)
	}
}

func w20_overall(out []w20_check) string {
	v := "WORKS"
	for _, c := range out {
		if c.verdict == "BROKEN" {
			return "BROKEN"
		}
		if c.verdict == "PARTIAL" {
			v = "PARTIAL"
		}
	}
	return v
}

type w20_stub struct {
	server *httptest.Server
	calls  atomic.Int64
	mu     sync.Mutex
	keys   []string
	states []string
}

func w20_start_so(t *testing.T) *w20_stub {
	t.Helper()
	s := &w20_stub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.keys = append(s.keys, r.Header.Get("x-api-key"))
		s.states = append(s.states, req.State)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, w20_answer)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// w20_home installs a hermetic HOME and points the real gate at the stub.
func w20_home(t *testing.T, so *w20_stub) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	oldOut := metricOut
	metricOut = io.Discard
	t.Cleanup(func() { metricOut = oldOut })
	routerReset()
	t.Cleanup(routerReset)
	oldEP := soEndpoint
	soEndpoint = so.server.URL
	t.Cleanup(func() { soEndpoint = oldEP })
	t.Setenv("WALSY_SO_ENDPOINT", so.server.URL)
	t.Setenv("EVREN_API_KEY", w20_key)
	return home
}

func w20_frame(id int, state, item string) []byte {
	params := map[string]any{
		"name": "w20_read", "state": state, "need_label": true, "no_scratch": true,
		"safe": true, "reversible": true, "opt_in": true, "authority": true,
		"calibrated": true, "phase": "before-model",
	}
	if item != "" {
		params["item"] = item
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params,
	})
	return append(b, '\n')
}

// w20_envelope is the real stateEnvelope shape used by route().
func w20_envelope(state, item string) string {
	b, _ := json.Marshal(map[string]any{
		"state": state, "method": "tools/call", "phase": "before-model",
		"authority": true, "opt_in": true, "calibrated": true,
		"need_label": true, "no_scratch": true, "safe": true,
		"reversible": true, "item": item,
	})
	return string(b)
}

func w20_scan(t *testing.T, home string, needles ...string) (int, []string) {
	t.Helper()
	files, leaks := 0, []string{}
	err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		for _, n := range needles {
			if bytes.Contains(b, []byte(n)) {
				leaks = append(leaks, p+" contains "+n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, leaks
}

// w20_chain recomputes H(n)=sha256(H(n-1)||canonicalJSON(receipt)) from disk.
func w20_chain(t *testing.T, home string) (int, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	if err != nil {
		return 0, false
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var prev []byte
	for i, line := range lines {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			return len(lines), false
		}
		if rec["seq"] != float64(i+1) || rec["prev"] != hex.EncodeToString(prev) {
			return len(lines), false
		}
		sum := sha256.Sum256(append(append([]byte{}, prev...), line...))
		prev = sum[:]
	}
	return len(lines), true
}

func w20_run_binary(t *testing.T, home, endpoint string) string {
	t.Helper()
	bin, err := filepath.Abs("walsy-system1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("shipped binary not found at %s: %v", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := []string{
		"HOME=" + home,
		"WALSY_SO_MODE=advisory",
		"WALSY_SO_ENDPOINT=" + endpoint,
		"EVREN_API_KEY=" + w20_key,
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "WALSY_") ||
			strings.HasPrefix(kv, "EVREN_API_KEY=") {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.CommandContext(ctx, bin, "-transport", "tools")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("shipped binary did not exit within 5s")
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Logf("shipped binary exit=%d out=%q", ee.ExitCode(), w20_clip(out.String()))
			return out.String()
		}
		t.Fatalf("shipped binary launch failed: %v", err)
	}
	t.Logf("shipped binary exit=0 out=%q", w20_clip(out.String()))
	return out.String()
}

func w20_clip(s string) string {
	if len(s) > 240 {
		return s[:240]
	}
	return s
}

func w20_arch() string {
	if p := strings.TrimSpace(os.Getenv("WALSY_SIM_ARCHMAP")); p != "" {
		return p
	}
	return w20_default_arch
}

// TestSimM4 drives real transport, handler and binary and observes where
// evidence is written and whether the M4 convention exists.
func TestSimM4EvidenceStorage(t *testing.T) {
	so := w20_start_so(t)
	home := w20_home(t, so)
	var out []w20_check

	// Real stdio transport -> real decision hub -> real receipt writer.
	frame := w20_frame(1, w20_sentinel, "")
	var toClient, toServer bytes.Buffer
	bridgeStdio(bytes.NewReader(frame), &toClient, &toServer, bytes.NewReader(nil))
	if so.calls.Load() == 0 {
		t.Fatal("M4 setup broke: SO stub received no POST from real bridgeStdio")
	}

	// Real dashboard handler: build/frame provenance surface.
	rec := httptest.NewRecorder()
	healthReq := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	healthReq.Host = "127.0.0.1" // loopbackHost rejects the default example.com
	NewDashboardHandler().ServeHTTP(rec, healthReq)
	var health map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &health) != nil {
		t.Fatalf("GET /api/health = %d %q", rec.Code, w20_clip(rec.Body.String()))
	}
	_, hasBuild := health["build_sha"]
	_, hasReceiptHash := health["receipt_sha256"]

	rp, err := receiptPath()
	if err != nil {
		t.Fatal(err)
	}
	pathHasConvention := false
	for _, part := range strings.Split(strings.ToLower(filepath.ToSlash(rp)), "/") {
		if part == "evidence" || part == "walsy-arch-map" {
			pathHasConvention = true
		}
	}

	n, chainOK := w20_chain(t, home)
	keyLeaked := false
	so.mu.Lock()
	for _, k := range so.keys {
		if k != "" {
			keyLeaked = true
		}
	}
	calls := so.calls.Load()
	so.mu.Unlock()
	files, leaks := w20_scan(t, home, w20_key, w20_sentinel, `"choice":"yes"`)
	switch {
	case !keyLeaked && calls > 0 && n > 0 && chainOK && len(leaks) == 0 && files > 0:
		w20_add(t, &out, "M4.1", "WORKS", fmt.Sprintf(
			"SO POSTs=%d, no key on loopback wire, receipt.chain lines=%d verify, %d HOME files carry no key/raw-SO/sentinel",
			calls, n, files))
	case !keyLeaked && calls > 0 && (n == 0 || !chainOK):
		w20_add(t, &out, "M4.1", "BROKEN", fmt.Sprintf("receipt chain unreadable or invalid: lines=%d verify=%v", n, chainOK))
	default:
		w20_add(t, &out, "M4.1", "BROKEN", fmt.Sprintf("redaction failed: calls=%d keyLeaked=%v leaks=%v", calls, keyLeaked, leaks))
	}

	switch {
	case hasBuild && hasReceiptHash && pathHasConvention:
		w20_add(t, &out, "M4.2", "WORKS", fmt.Sprintf(
			"real /api/health exposes build_sha=%v receipt_sha256=%v and receiptPath()=%s uses walsy-arch-map/evidence convention",
			health["build_sha"], health["receipt_sha256"], rp))
	case hasBuild && hasReceiptHash && !pathHasConvention:
		w20_add(t, &out, "M4.2", "PARTIAL", fmt.Sprintf(
			"real /api/health exposes build_sha=%v receipt_sha256=%v, but receiptPath()=%s has no walsy-arch-map/evidence convention",
			health["build_sha"], health["receipt_sha256"], rp))
	default:
		w20_add(t, &out, "M4.2", "BROKEN", fmt.Sprintf(
			"health build_sha=%v receipt_sha256=%v receiptPath=%s pathHasConvention=%v",
			hasBuild, hasReceiptHash, rp, pathHasConvention))
	}

	arch := w20_arch()
	want := []string{
		"evidence/build/verify-K12.txt",
		"evidence/build/walsy-system1.digest.json",
		"evidence/build/b01-meta.txt",
		"evidence/frame/relay56-TRANSCRIPT.txt",
		"evidence/frame/equivalence-HEAD.txt",
		"evidence/hash-chain/receipt-sample.jsonl",
	}
	found, missing := []string{}, []string{}
	for _, rel := range want {
		if st, e := os.Stat(filepath.Join(arch, rel)); e == nil && st.Mode().IsRegular() && st.Size() > 0 {
			found = append(found, rel)
		} else {
			missing = append(missing, rel)
		}
	}
	// Exact genuine SO transcripts at the arch-map root; a bare so_*.json
	// count would accept empty or renamed placeholders.
	soWant := []string{"so_final.json", "so_final2.json"}
	soFound, soMissing := []string{}, []string{}
	for _, name := range soWant {
		if st, e := os.Stat(filepath.Join(arch, name)); e == nil && st.Mode().IsRegular() && st.Size() > 0 {
			soFound = append(soFound, name)
		} else {
			soMissing = append(soMissing, name)
		}
	}
	w20_run_binary(t, home, so.server.URL)
	filesAfter, _ := w20_scan(t, home, w20_key, w20_sentinel, `"choice":"yes"`)
	switch {
	case len(found) == len(want) && len(soFound) == len(soWant) && filesAfter == files:
		w20_add(t, &out, "M4.3", "WORKS", fmt.Sprintf(
			"arch=%s has %d/%d non-empty evidence artifacts and %v SO transcripts; shipped binary wrote 0 new HOME files",
			arch, len(found), len(want), soFound))
	case len(soFound) > 0 && len(found) < len(want) && filesAfter == files:
		w20_add(t, &out, "M4.3", "PARTIAL", fmt.Sprintf(
			"arch=%s has %v SO transcripts, found %d/%d artifacts, missing=%v; shipped binary wrote 0 new HOME files",
			arch, soFound, len(found), len(want), missing))
	default:
		w20_add(t, &out, "M4.3", "BROKEN", fmt.Sprintf(
			"arch=%s soFound=%v soMissing=%v found=%v/%d homeFiles=%d->%d",
			arch, soFound, soMissing, found, len(want), files, filesAfter))
	}
	t.Logf("M4 OVERALL: %s", w20_overall(out))
}

// TestSimM5 feeds owner metadata through the real ledger, final audit and
// route, then inspects the assignment table the items actually use.
func TestSimM5OwnershipFields(t *testing.T) {
	so := w20_start_so(t)
	_ = w20_home(t, so)
	var out []w20_check

	doneState := `{"todo":["M4"],"done":["M4"],"who":"research","when":"B70"}`
	openState := `{"todo":["M4"],"done":[]}`
	lDone := todoLedgerFrom(doneState)
	lOpen := todoLedgerFrom(openState)
	aDone := finalAudit(doneState)
	aOpen := finalAudit(openState)
	rDone := route(w20_envelope(doneState, "antishirk.todo_ledger"), nil)
	rOpen := route(w20_envelope(openState, "antishirk.todo_ledger"), nil)
	if lDone.Verdict.Choice == "complete" && lOpen.Verdict.Choice == "do_more" &&
		aDone.OK && !aOpen.OK && rDone.Reason == "ledger-complete" &&
		rDone.Decision == decPin && rOpen.Reason == "ledger-open" {
		w20_add(t, &out, "M5.1", "WORKS", fmt.Sprintf(
			"real ledger+audit+route: done -> %s/%s, open -> %s/%s (no SO call)",
			lDone.Verdict.Choice, rDone.Reason, lOpen.Verdict.Choice, rOpen.Reason))
	} else {
		w20_add(t, &out, "M5.1", "BROKEN", fmt.Sprintf(
			"ledger=%q/%q audit=%v/%v route=%q/%q",
			lDone.Verdict.Choice, lOpen.Verdict.Choice, aDone.OK, aOpen.OK, rDone.Reason, rOpen.Reason))
	}

	owned := `{"todo":["M4"],"done":["M4"],"who":"research","when":"B70"}`
	unowned := `{"todo":["M4"],"done":["M4"],"who":"","when":""}`
	var dec, led auditState
	_ = json.Unmarshal([]byte(owned), &dec)
	_ = json.Unmarshal([]byte(owned), &led)
	decRound, _ := json.Marshal(dec)
	ledRound, _ := json.Marshal(led)
	dropsWhoWhen := !strings.Contains(string(decRound), `"who"`) &&
		!strings.Contains(string(decRound), `"when"`) &&
		!strings.Contains(string(ledRound), `"who"`) &&
		!strings.Contains(string(ledRound), `"when"`)
	sameVerdict := finalAudit(owned).OK && finalAudit(unowned).OK &&
		todoLedgerFrom(owned).Verdict.Choice == "complete" &&
		todoLedgerFrom(unowned).Verdict.Choice == "complete"
	if dropsWhoWhen && sameVerdict {
		w20_add(t, &out, "M5.2", "BROKEN", fmt.Sprintf(
			"auditState/todoLedgerState drop who+when (%s / %s); owned and unowned states both pass audit and ledger",
			string(decRound), string(ledRound)))
	} else {
		w20_add(t, &out, "M5.2", "WORKS", fmt.Sprintf(
			"owner metadata survives real types or changes verdict: dec=%s led=%s", decRound, ledRound))
	}

	arch := w20_arch()
	body, e := os.ReadFile(filepath.Join(arch, "TODO.md"))
	if e != nil {
		w20_add(t, &out, "M5.3", "BROKEN", fmt.Sprintf("assignment table unreadable at %s: %v", arch, e))
	} else {
		header := ""
		for _, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "| ID |") {
				header = strings.TrimSpace(line)
				break
			}
		}
		low := strings.ToLower(header)
		hasCols := strings.Contains(low, "who") && strings.Contains(low, "when") && strings.Contains(low, "done")
		if !hasCols {
			w20_add(t, &out, "M5.3", "BROKEN", fmt.Sprintf(
				"assignment table header %q has no Who/When/Done columns", header))
		} else {
			w20_add(t, &out, "M5.3", "WORKS", fmt.Sprintf("assignment table header %q carries Who/When/Done", header))
		}
	}
	t.Logf("M5 OVERALL: %s", w20_overall(out))
}
