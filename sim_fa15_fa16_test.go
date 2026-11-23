package main

// Genuine simulation for MANIFEST FA15 and FA16. Static-only: no live SO, no
// live credential, no production edit. Each test drives real production code
// (decision hub, receipt chain, build command, transports, dashboard handler,
// tools server, verify shell) and asserts the behavior the item requires, so a
// missing feature fails here instead of passing by construction.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	faW32SOYes      = `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}},"usage":{"evren":{"credits":1}}}`
	faW32SOComplete = `{"answers":{"release":{"type":"choice","choice":"complete","confidence":0.95}},"usage":{"evren":{"credits":1}}}`
)

func faW32Env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "fa-sim-key") // synthetic; no real key is read
	routerReset()
	t.Cleanup(routerReset)
	resetLogs()
	t.Cleanup(resetLogs)
	return home
}

func faW32SO(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	old := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = old; srv.Close() })
	return srv
}

func faW32Frame(state string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fa_probe","arguments":{},"state":"` +
		state + `","need_label":true,"no_scratch":true,"safe":true,"reversible":true,` +
		`"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"}}` + "\n")
}

func faW32Health(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "127.0.0.1:8080"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("health JSON: %v: %s", err, rec.Body.String())
	}
	return out
}

// FA15.1: /api/health must be a reproducible fingerprint: items digest from
// the embedded bytes, receipt digest from items + active prompt set + build SHA,
// and no prompt body on the wire.
func TestSimFA15HealthFingerprintReproducible(t *testing.T) {
	faW32Env(t)
	const body = "FA15_PROMPT_BODY_ALPHA"
	if err := savePrompts([]Prompt{{Name: "fa15-a", Text: body}}); err != nil {
		t.Fatalf("savePrompts: %v", err)
	}
	h := NewDashboardHandler()
	first := faW32Health(t, h)
	second := faW32Health(t, h)

	items := sha256.Sum256(decisionItemsJSON)
	if got, want := first["items_sha256"], hex.EncodeToString(items[:]); got != want {
		t.Fatalf("FA15 items_sha256=%v want %s", got, want)
	}
	sum1, ok := first["receipt_sha256"].(string)
	if !ok || len(sum1) != 64 {
		t.Fatalf("FA15 receipt_sha256=%v want 64-hex", first["receipt_sha256"])
	}
	buildSHA, _ := first["build_sha"].(string)
	calc := sha256.New()
	_, _ = calc.Write(decisionItemsJSON)
	_, _ = fmt.Fprintf(calc, "%d:%s%d:%s", len("fa15-a"), "fa15-a", len(body), body)
	_, _ = calc.Write([]byte(buildSHA))
	if want := hex.EncodeToString(calc.Sum(nil)); sum1 != want {
		t.Fatalf("FA15 receipt_sha256=%s want sha256(items+prompts+build)=%s", sum1, want)
	}
	if first["receipt_sha256"] != second["receipt_sha256"] {
		t.Fatal("FA15 fingerprint not reproducible across calls")
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), body) {
		t.Fatal("FA15 prompt body leaked into /api/health")
	}
	if err := savePrompts([]Prompt{{Name: "fa15-a", Text: "FA15_PROMPT_BODY_BETA"}}); err != nil {
		t.Fatal(err)
	}
	changed := faW32Health(t, h)
	if changed["receipt_sha256"] == sum1 {
		t.Fatal("FA15 prompt set change did not rebind receipt_sha256")
	}
	if changed["items_sha256"] != first["items_sha256"] {
		t.Fatal("FA15 items digest changed with prompts")
	}
}

// FA15.2: each receipt must carry the reproducibility triple so a ledger line
// can be checked against the binary and the item/prompt set that produced it.
func TestSimFA15ReceiptBindsProvenance(t *testing.T) {
	home := faW32Env(t)
	faW32SO(t, faW32SOYes)
	if err := savePrompts([]Prompt{{Name: "fa15-r", Text: "FA15_RECEIPT_PROMPT"}}); err != nil {
		t.Fatal(err)
	}
	d := newDecisionBridge()
	r := d.decideFrame(faW32Frame("fa15 receipt provenance check"))
	if !receiptWorthy(r) {
		t.Fatalf("FA15 frame produced no receipt-worthy result: %+v", r)
	}
	path := filepath.Join(home, routerDir, "receipt.chain")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("FA15 receipt.chain: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		t.Fatal("FA15 receipt chain empty")
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatalf("FA15 receipt JSON: %v: %s", err, lines[len(lines)-1])
	}
	h := healthFingerprint()
	want := map[string]any{
		"items_sha256":   h["items_sha256"],
		"receipt_sha256": h["receipt_sha256"],
		"build_sha":      h["build_sha"],
	}
	var missing []string
	for key, value := range want {
		got, ok := row[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		if got != value {
			t.Fatalf("FA15 receipt %s=%v want %v", key, got, value)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		t.Fatalf("FA15 BROKEN: receipt omits %v; row=%s", missing, lines[len(lines)-1])
	}
}

// FA15.3: rebuild must be CGO_ENABLED=0 from a clean HEAD snapshot. A stub
// toolchain records the real command line and its working directory; a real
// temporary git repo supplies HEAD and a dirty working tree to distinguish the
// two. No live compiler and no real repository is touched.
func TestSimFA15HeadRebuild(t *testing.T) {
	home := faW32Env(t)
	src := filepath.Join(home, "src")
	if err := os.MkdirAll(src, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	_ = git("init", "-q")
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module sim\n\ngo 1.26\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\nfunc main(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = git("add", ".")
	_ = git("-c", "user.email=sim@example.invalid", "-c", "user.name=sim", "commit", "-q", "-m", "sim-head")
	head := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n// dirty\nfunc main(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if st := git("status", "--porcelain"); st == "" {
		t.Fatal("FA15 sim setup failed: working tree is clean")
	}

	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "build.log")
	script := "#!/bin/sh\n" +
		"printf 'CGO_ENABLED=%s|GOOS=%s|GOARCH=%s|PWD=%s|%s\\n' \"$CGO_ENABLED\" \"$GOOS\" \"$GOARCH\" \"$PWD\" \"$*\" >> \"$FA_BUILD_LOG\"\n" +
		"out=''\nwhile [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = '-o' ]; then out=\"$2\"; shift 2; else shift; fi\ndone\n" +
		"mkdir -p \"$(dirname \"$out\")\"\nprintf 'FA15-IMAGE' > \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FA_BUILD_LOG", logPath)
	if err := os.MkdirAll(filepath.Join(src, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := runGoBuild(context.Background(), src, "dist/candidate", "darwin", "arm64"); err != nil {
		t.Fatalf("FA15 runGoBuild: %v %s", err, out)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := string(log)
	for _, want := range []string{"CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64", "-trimpath", "-buildvcs=true", "-o dist/candidate"} {
		if !strings.Contains(line, want) {
			t.Fatalf("FA15 build command missing %q: %s", want, line)
		}
	}
	i := strings.Index(line, "PWD=")
	cwd := strings.TrimSpace(strings.SplitN(line[i+len("PWD="):], "|", 2)[0])
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = cwd
	statusCmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
	status, stErr := statusCmd.CombinedOutput()
	if stErr != nil || len(strings.TrimSpace(string(status))) != 0 {
		t.Fatalf("FA15 BROKEN: rebuild ran in a dirty tree (%s); HEAD rebuild needs a clean git archive/worktree at %s", cwd, head)
	}
	revCmd := exec.Command("git", "rev-parse", "HEAD")
	revCmd.Dir = cwd
	revCmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
	rev, revErr := revCmd.CombinedOutput()
	if revErr != nil || strings.TrimSpace(string(rev)) != head {
		t.Fatalf("FA15 rebuild source revision=%q want HEAD=%q", strings.TrimSpace(string(rev)), head)
	}
}

// FA15.4: the same in-scope frame forwarded through stdio and through the HTTP
// MCP handler must survive byte-for-byte, with the same fingerprint before and
// after both transports.
func TestSimFA15FrameEquivalenceTransports(t *testing.T) {
	faW32Env(t)
	faW32SO(t, faW32SOYes)
	frames := [][]byte{
		faW32Frame("fa15 equivalence one"),
		faW32Frame("fa15 equivalence two"),
	}
	before := healthFingerprint()["receipt_sha256"]

	var clientOut, upstreamIn bytes.Buffer
	reply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")
	bridgeStdio(bytes.NewReader(bytes.Join(frames, nil)), &clientOut, &upstreamIn, bytes.NewReader(reply))
	stdioForward := upstreamIn.String()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		_, _ = w.Write(b.Bytes())
	}))
	defer up.Close()
	h := mcpHTTPHandlerUpstream(up.URL)
	var httpForward strings.Builder
	for _, frame := range frames {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
		req.Header.Set("Content-Type", "application/json")
		req.Host = "127.0.0.1:8080"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("FA15 HTTP frame status %d: %s", rec.Code, rec.Body.String())
		}
		_, _ = httpForward.WriteString(rec.Body.String())
	}
	if stdioForward != httpForward.String() {
		t.Fatalf("FA15 BROKEN: transport frame divergence\nstdio=%q\nhttp=%q", stdioForward, httpForward.String())
	}
	if want := string(bytes.Join(frames, nil)); stdioForward != want {
		t.Fatalf("FA15 stdio forwarded %q want verbatim %q", stdioForward, want)
	}
	if after := healthFingerprint()["receipt_sha256"]; after != before {
		t.Fatalf("FA15 transport changed fingerprint: %v -> %v", before, after)
	}
}

// FA15.5: run the real verify.sh from a temporary copy with stubbed tools. The
// script logic, its stage count and its PASS verdict are observed, not assumed;
// no real build overwrites the repository binary.
func TestSimFA15VerifyScriptHarness(t *testing.T) {
	home := faW32Env(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scriptBytes, err := os.ReadFile(filepath.Join(root, "scripts", "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fakeRoot := filepath.Join(home, "verifyrepo")
	if err := os.MkdirAll(filepath.Join(fakeRoot, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(fakeRoot, "scripts", "verify.sh")
	if err := os.WriteFile(scriptPath, scriptBytes, 0700); err != nil {
		t.Fatal(err)
	}
	// verify.sh expands a files array under set -u; give it one ASCII source.
	if err := os.WriteFile(filepath.Join(fakeRoot, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Stage 8 verifies a real detached signature. Copy both artifacts so the
	// check runs for real instead of being stubbed out.
	for _, name := range []string{"decision_items.json", "decision_items.json.sha256.sig"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeRoot, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(home, "verifybin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "verify.log")
	goStub := "#!/bin/sh\nprintf 'CGO_ENABLED=%s|%s\\n' \"$CGO_ENABLED\" \"$*\" >> \"$FA_VERIFY_LOG\"\nexit 0\n"
	lrecStub := "#!/bin/sh\nprintf '{}\\n'\n"
	for name, body := range map[string]string{
		"go": goStub, "gofmt": "#!/bin/sh\nexit 0\n", "lrecon": lrecStub,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, scriptPath)
	cmd.Dir = fakeRoot
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LRECON="+filepath.Join(bin, "lrecon"), "FA_VERIFY_LOG="+logPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FA15 verify.sh failed: %v: %s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "== 10/10 verdict ==") || !strings.HasSuffix(strings.TrimSpace(text), "PASS") {
		t.Fatalf("FA15 verify.sh did not keep 10 stages and PASS: %s", text)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	stages := string(log)
	for _, want := range []string{"CGO_ENABLED=0|build -trimpath -buildvcs=false", "CGO_ENABLED=1|test -race -count=1 ./...", "vet ./..."} {
		if !strings.Contains(stages, want) {
			t.Fatalf("FA15 verify.sh weakened a gate %q: %s", want, stages)
		}
	}
}

// FA16.1: the product must be discoverable under all three release names in
// operator-facing surfaces (MCP tools/list and /api/health).
func TestSimFA16NamingAliases(t *testing.T) {
	faW32Env(t)
	var out bytes.Buffer
	runToolsServer(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out)
	healthRaw, _ := json.Marshal(faW32Health(t, NewDashboardHandler()))
	surfaces := out.String() + string(healthRaw)
	var missing []string
	for _, alias := range []string{"systemone", "system one", "Smart Router (System One)"} {
		if !strings.Contains(strings.ToLower(surfaces), strings.ToLower(alias)) {
			missing = append(missing, alias)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("FA16 BROKEN: release aliases absent from tools/list and /api/health: %v", missing)
	}
}

// FA16.2: release provenance must state PR #4975 as unmerged while recording
// that its code is present in v0.5.109. Health is the only machine-readable
// provenance surface in this tree.
func TestSimFA16PR4975Provenance(t *testing.T) {
	faW32Env(t)
	h := faW32Health(t, NewDashboardHandler())
	rel, ok := h["release"].(map[string]any)
	if !ok {
		t.Fatalf("FA16 BROKEN: /api/health has no release provenance object: %v", h)
	}
	version, _ := rel["version"].(string)
	if !strings.Contains(version, "0.5.109") {
		t.Fatalf("FA16 release.version=%q want v0.5.109 lineage", version)
	}
	pr, ok := rel["pr"].(map[string]any)
	if !ok {
		t.Fatalf("FA16 BROKEN: release.pr missing: %v", rel)
	}
	if n, _ := pr["number"].(float64); n != 4975 {
		t.Fatalf("FA16 release.pr.number=%v want 4975", pr["number"])
	}
	if got, present := pr["merged_at"]; !present || got != nil {
		t.Fatalf("FA16 release.pr.merged_at=%v want null (unmerged PR)", got)
	}
	if pr["code_present"] != true {
		t.Fatalf("FA16 release.pr.code_present=%v want true for v0.5.109 code", pr["code_present"])
	}
}

// FA16.3: E2 stable release must be a real gate: four attestations plus an SO
// complete >=0.8, with a stable verdict that flips only when all are present.
func TestSimFA16StableReleaseGate(t *testing.T) {
	faW32Env(t)
	faW32SO(t, faW32SOComplete)
	h := NewDashboardHandler()
	post := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/release", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1:8080")
		req.Host = "127.0.0.1:8080"
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	all := `{"criteria":{"f1_build_equivalence":true,"verify_pass":true,"so_final_complete":true,"m1_approval":true}}`
	code, body := post(all)
	if code == http.StatusNotFound {
		t.Fatal("FA16 BROKEN: no machine-checkable stable release gate (/api/release 404)")
	}
	if code != http.StatusOK {
		t.Fatalf("FA16 release gate status %d: %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("FA16 release gate JSON: %v: %s", err, body)
	}
	if got["stable_release"] != true {
		t.Fatalf("FA16 all criteria met but stable_release=%v: %s", got["stable_release"], body)
	}
	partial := `{"criteria":{"f1_build_equivalence":true,"verify_pass":true,"so_final_complete":true,"m1_approval":false}}`
	code, body = post(partial)
	if code != http.StatusOK {
		t.Fatalf("FA16 release gate status %d: %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &got)
	if got["stable_release"] != false {
		t.Fatalf("FA16 missing M1 approval still released stable: %s", body)
	}
}
