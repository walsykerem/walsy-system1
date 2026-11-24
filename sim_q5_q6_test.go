package main

import (
	"bytes"
	"crypto/sha256"
	"debug/gosym"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// Static-only simulation for MANIFEST B23 items Q5 and Q6.
// No target execution, no live process inspection, no real SO call, no key read.
// The report is the artifact under test; the binary and the in-process handlers
// are the ground truth used to check what the report is allowed to claim.
const (
	w23_FuncCount  = 11136
	w23_OldFUNAddr = 0x12ad0
)

func w23_ReadReport(t *testing.T) string {
	t.Helper()
	p := os.Getenv("WALSY_RAPOR")
	if p == "" {
		p = "RAPOR.md"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read static report: %v", err)
	}
	return string(b)
}

func w23_ReadBinary(t *testing.T) ([]byte, *macho.File) {
	t.Helper()
	raw, err := os.ReadFile(simMachOBinary(t))
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	_, digest := simReadDigest(t, simMachODigest())
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != digest.Target.FileSHA256 {
		t.Fatalf("binary changed since digest: sha256=%s want %s", got, digest.Target.FileSHA256)
	}
	if digest.Target.Size != int64(len(raw)) {
		t.Fatalf("digest target.size=%d, binary size=%d", digest.Target.Size, len(raw))
	}
	mf, err := macho.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse Mach-O: %v", err)
	}
	return raw, mf
}

// w23_FencedBlocks returns the contents of every fenced code block in order.
func w23_FencedBlocks(report string) []string {
	var blocks []string
	in := false
	var cur strings.Builder
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if in {
				blocks = append(blocks, cur.String())
				cur.Reset()
			}
			in = !in
			continue
		}
		if in {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	return blocks
}

// w23_IsPipeline distinguishes a data-flow diagram from other fenced blocks
// (digest JSON, skill contract) by requiring both flow glyphs and pipeline
// stage names. This is the detector whose output Q5 reduces from two to one.
func w23_IsPipeline(block string) bool {
	arrows := 0
	for _, tok := range []string{"\u2502", "\u25bc", "\u251c\u2500", "\u2514\u2500", "->", "\u2192"} {
		if strings.Contains(block, tok) {
			arrows++
		}
	}
	stages := 0
	for _, tok := range []string{"decodeFrame", "ADVISORY", "ENFORCING", "[transport:", "upstream", "SO endpoint"} {
		if strings.Contains(block, tok) {
			stages++
		}
	}
	return arrows >= 2 && stages >= 2
}

func w23_Pipelines(report string) []string {
	var out []string
	for _, b := range w23_FencedBlocks(report) {
		if w23_IsPipeline(b) {
			out = append(out, b)
		}
	}
	return out
}

// TestSimQ5 checks Q5 ("delete one of two pipeline diagrams") against the
// report, and makes the surviving diagram's two hops observable in-process:
// a real JSON-RPC frame through mcpHTTPHandler, and a real SO POST to a stub.
func TestSimQ5(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	_, _ = w23_ReadBinary(t)

	report := w23_ReadReport(t)
	pipes := w23_Pipelines(report)
	if len(pipes) != 1 {
		t.Fatalf("Q5: pipeline diagrams = %d, want 1", len(pipes))
	}
	for _, tok := range []string{"[transport:", "decodeFrame", "upstream", "SO endpoint", "ADVISORY", "ENFORCING"} {
		if !strings.Contains(pipes[0], tok) {
			t.Errorf("Q5: surviving diagram lost stage %q", tok)
		}
	}

	// Hop 1: the diagram's "loopback HTTP POST /" path echoes a real frame.
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("build frame request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST frame: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read frame response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, frame) {
		t.Fatalf("Q5: frame hop = %d %s, want 200 + verbatim echo", resp.StatusCode, got)
	}

	// Hop 2: the diagram's "HTTP POST SO endpoint" is a real POST, not prose.
	var posts atomic.Int64
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("SO method = %s", r.Method)
		}
		var q struct {
			Model string `json:"model"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Errorf("decode SO body: %v", err)
		}
		if q.Model != "clef" || !strings.HasPrefix(q.State, "sim pipeline state\nso_session_id=") ||
			!strings.Contains(q.State, "\nso_prompt_cache_key=") {
			t.Errorf("unexpected SO body: %+v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.92}}}`)
	}))
	defer oracle.Close()
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = oracle.URL, oracle.Client()
	defer func() { soEndpoint, soClient = oldEndpoint, oldClient }()
	t.Setenv("EVREN_API_KEY", "sim-not-a-secret")

	d := askSOQOnce("sim pipeline state", soQuestion{Type: "choice", Criteria: map[string]string{
		"yes": "admit", "no": "pass", "other": "ambiguous",
	}})
	if posts.Load() != 1 {
		t.Fatalf("Q5: SO POST count = %d, want 1", posts.Load())
	}
	if d.Choice != "yes" || d.Conf != 0.92 {
		t.Fatalf("Q5: SO decision = %+v", d)
	}

	// Negative controls: the detector must catch duplication and accept the fix.
	two := report + "\n```text\n" + pipes[0] + "```\n"
	if n := len(w23_Pipelines(two)); n != 2 {
		t.Fatalf("Q5 detector missed a second diagram: got %d", n)
	}
	if n := len(w23_Pipelines("no fenced blocks here")); n != 0 {
		t.Fatalf("Q5 detector invented a diagram: got %d", n)
	}
	decoy := "```\n{\"TARGET\":\"...\",\"KIND\":\"macho\"}\n```\n"
	if n := len(w23_Pipelines(decoy)); n != 0 {
		t.Fatalf("Q5 detector counted a non-diagram block: got %d", n)
	}
}

// w23_UnlabeledFUN reports report lines that cite a FUN_* name without
// saying it is a pclntab name. Q6 accepts deletion or an explicit label.
func w23_UnlabeledFUN(report string) []string {
	var bad []string
	for _, line := range strings.Split(report, "\n") {
		if !strings.Contains(line, "FUN_") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "pclntab") {
			continue
		}
		bad = append(bad, strings.TrimSpace(line))
	}
	return bad
}

// TestSimQ6 checks Q6 ("delete the list_procedures FUN_* finding or label it
// as a pclntab name"). The binary side proves the label is truthful: Go names
// are recovered from __gopclntab, not from the stripped Mach-O symbol table.
func TestSimQ6(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")

	_, mf := w23_ReadBinary(t)
	sec := mf.Section("__gopclntab")
	if sec == nil || sec.Seg != "__TEXT" || sec.Size == 0 {
		t.Fatalf("Q6: binary has no __TEXT,__gopclntab section")
	}
	data, err := sec.Data()
	if err != nil {
		t.Fatalf("read __gopclntab: %v", err)
	}
	if len(data) < 8 || !bytes.Equal(data[:4], []byte{0xf1, 0xff, 0xff, 0xff}) {
		t.Fatalf("Q6: unexpected pclntab magic % x", data[:w23_min(8, len(data))])
	}
	tab, err := gosym.NewTable(nil, gosym.NewLineTable(data, sec.Addr))
	if err != nil {
		t.Fatalf("parse pclntab: %v", err)
	}
	if len(tab.Funcs) != w23_FuncCount {
		t.Fatalf("Q6: pclntab funcs = %d, want %d (recon digest)", len(tab.Funcs), w23_FuncCount)
	}
	foundMain := false
	staleAddr := false
	for _, f := range tab.Funcs {
		if f.Name == "main.main" {
			foundMain = true
		}
		// The deleted finding cited FUN_00012ad0 main.main. If that address
		// were a real Go name, the pclntab label would have been truthful.
		if f.Entry&0xffffff == w23_OldFUNAddr {
			staleAddr = true
		}
	}
	if !foundMain {
		t.Fatal("Q6: no exact main.main name in pclntab; FUN_ names cannot be pclntab names")
	}
	if staleAddr {
		t.Error("Q6: FUN_00012ad0 maps to a pclntab function; deletion is not the only truthful fix")
	}
	report := w23_ReadReport(t)
	if hits := w23_UnlabeledFUN(report); len(hits) != 0 {
		for _, h := range hits {
			t.Errorf("Q6: unlabeled FUN_* finding: %s", h)
		}
	}
	if !strings.Contains(report, "pclntab") {
		t.Error("Q6: report dropped the truthful pclntab attribution entirely")
	}

	// Negative controls: old finding fails; label and deletion both pass.
	oldLine := "- list_procedures found FUN_00012ad0 main.main (procedure name)"
	if got := w23_UnlabeledFUN(oldLine); len(got) != 1 {
		t.Fatalf("Q6 detector missed old finding: %v", got)
	}
	labeled := oldLine + " -- pclntab name, not a Mach-O symbol"
	if got := w23_UnlabeledFUN(labeled); len(got) != 0 {
		t.Fatalf("Q6 detector rejected pclntab label: %v", got)
	}
	if got := w23_UnlabeledFUN("- procedures are pclntab names only"); len(got) != 0 {
		t.Fatalf("Q6 detector rejected deletion: %v", got)
	}
}

func w23_min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
