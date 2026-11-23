package main

import (
	"bytes"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// Static-only simulation for MANIFEST B22 items Q3 and Q4.
// No target binary execution, no live process inspection, no real SO call.
const (
	simBinaryPath = "/Users/keremurhan/walsy-system1/walsy-system1"
	simBinarySHA  = "aeb9598d8ed7b9c42d2c77caab41296363d0a5c3bf902813027327f3013bfb4d"
	simAESOffset  = 0x8914e0
	simAESBytes   = "637c777bf26b6fc5"
	simAESSegment = "__DATA,__noptrdata"
)

var simSegmentRe = regexp.MustCompile(`__[A-Za-z0-9_]+,__[A-Za-z0-9_]+`)

func simReadReport(t *testing.T) string {
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

// simQ3BadAESLines flags any aes_sbox line that names a segment other than the
// one the analyzed Mach-O actually carries the constant in. A line with no
// segment name is accepted: Q3 allows deleting the label.
func simQ3BadAESLines(report, wantSegment string) []string {
	var bad []string
	for _, line := range strings.Split(report, "\n") {
		if !strings.Contains(line, "aes_sbox") {
			continue
		}
		if m := simSegmentRe.FindString(line); m != "" && m != wantSegment {
			bad = append(bad, strings.TrimSpace(line))
		}
	}
	return bad
}

// TestSimQ3 proves the report's aes_sbox segment claim against the real binary
// using stdlib Mach-O parsing, then proves the checker rejects the old label.
func TestSimQ3(t *testing.T) {
	raw, err := os.ReadFile(simBinaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != simBinarySHA {
		t.Fatalf("binary changed since recon: sha256=%s want %s", got, simBinarySHA)
	}
	mf, err := macho.NewFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse Mach-O: %v", err)
	}
	section := ""
	for _, s := range mf.Sections {
		// BSS sections report offset 0 and have no bytes on disk.
		if s.Offset == 0 {
			continue
		}
		if uint64(s.Offset) <= simAESOffset && simAESOffset < uint64(s.Offset)+s.Size {
			section = s.Seg + "," + s.Name
			break
		}
	}
	if section == "" {
		t.Fatalf("offset 0x%x belongs to no section", simAESOffset)
	}
	buf := make([]byte, len(simAESBytes)/2)
	if _, err := bytes.NewReader(raw).ReadAt(buf, simAESOffset); err != nil {
		t.Fatalf("read bytes at 0x%x: %v", simAESOffset, err)
	}
	if got := hex.EncodeToString(buf); got != simAESBytes {
		t.Fatalf("bytes at 0x%x = %s want %s", simAESOffset, got, simAESBytes)
	}
	if section != simAESSegment {
		t.Fatalf("aes_sbox at 0x%x lives in %s, not %s", simAESOffset, section, simAESSegment)
	}

	if bad := simQ3BadAESLines(simReadReport(t), section); len(bad) != 0 {
		for _, line := range bad {
			t.Errorf("report mislabels aes_sbox: %s", line)
		}
	}

	oldLine := "0x88d4e0: 637c777bf26b6fc5 (digest constant aes_sbox __TEXT,__rodata; xxd at file offset matches)"
	if got := simQ3BadAESLines(oldLine, simAESSegment); len(got) != 1 {
		t.Fatalf("checker failed to reject old label: %v", got)
	}
	fixedLine := strings.Replace(oldLine, "__TEXT,__rodata", simAESSegment, 1)
	if got := simQ3BadAESLines(fixedLine, simAESSegment); len(got) != 0 {
		t.Fatalf("checker rejected corrected label: %v", got)
	}
	deletedLine := "AES S-box 637c777b f26b6fc5 at 0x88d4e0"
	if got := simQ3BadAESLines(deletedLine, simAESSegment); len(got) != 0 {
		t.Fatalf("checker rejected label deletion: %v", got)
	}
}

type simLivePattern struct {
	why   string
	token string
}

var simLivePatterns = []simLivePattern{
	{"pid", "63806"},
	{"live-proof wording", "live proof"},
	{"UI claim", "ADD PROMPT"},
	{"hedge", "unless stated"},
}

// simQ4LiveProofHits finds runtime-proof markers that do not belong in a
// static report. Reasons are collected per line so the negative control must
// trip every marker, not just the first one.
func simQ4LiveProofHits(report string) []string {
	var hits []string
	for _, line := range strings.Split(report, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, p := range simLivePatterns {
			if strings.Contains(line, p.token) {
				hits = append(hits, p.why+": "+trimmed)
			}
		}
		if strings.Contains(line, "/api/prompts") && strings.Contains(line, "200") {
			hits = append(hits, "endpoint 200: "+trimmed)
		}
	}
	return hits
}

// TestSimQ4 simulates both runtime claims in-process (real dashboard handler,
// real SO POST transport against an httptest oracle), then verifies the static
// report neither repeats those claims nor drops its static-only evidence.
func TestSimQ4(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")

	// The /api/prompts and /ui/prompts capability is reproducible in-process;
	// that does not prove anything about any previously running PID.
	dash := httptest.NewServer(NewDashboardHandler())
	defer dash.Close()
	code, body := simQ4Get(t, dash.Client(), dash.URL+"/api/prompts")
	if code != http.StatusOK {
		t.Fatalf("GET /api/prompts = %d", code)
	}
	var prompts []Prompt
	if err := json.Unmarshal([]byte(body), &prompts); err != nil {
		t.Fatalf("GET /api/prompts body %q: %v", body, err)
	}
	code, page := simQ4Get(t, dash.Client(), dash.URL+"/ui/prompts")
	if code != http.StatusOK {
		t.Fatalf("GET /ui/prompts = %d", code)
	}
	if !strings.Contains(page, "ADD PROMPT") {
		t.Fatal("GET /ui/prompts lost ADD PROMPT")
	}

	// A real SO POST is observable, so a static report cannot cite one.
	var posts atomic.Int64
	oracle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("SO method = %s", r.Method)
		}
		if r.Header.Get("x-api-key") != "sim-not-a-secret" {
			t.Error("SO stub got no test key")
		}
		var req struct {
			Model     string `json:"model"`
			State     string `json:"state"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode SO request: %v", err)
		}
		if req.Model != "clef" || req.State != "sim state" || req.Questions["gate"].Type != "choice" {
			t.Errorf("unexpected SO request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.91}}}`)
	}))
	defer oracle.Close()
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = oracle.URL, oracle.Client()
	defer func() { soEndpoint, soClient = oldEndpoint, oldClient }()
	t.Setenv("EVREN_API_KEY", "sim-not-a-secret")

	d := askSOQOnce("sim state", soQuestion{Type: "choice", Criteria: map[string]string{
		"yes": "admit", "no": "pass", "other": "ambiguous",
	}})
	if posts.Load() != 1 {
		t.Fatalf("SO POST count = %d want 1", posts.Load())
	}
	if d.Choice != "no" || d.Conf != 0.91 {
		t.Fatalf("SO decision = %+v", d)
	}

	report := simReadReport(t)
	if hits := simQ4LiveProofHits(report); len(hits) != 0 {
		for _, h := range hits {
			t.Errorf("live-proof residue in static report: %s", h)
		}
	}
	for _, keep := range []string{"KeepAlive+RunAtLoad", "vcs.modified=true", "No live SO call"} {
		if !strings.Contains(report, keep) {
			t.Errorf("static evidence lost: %q", keep)
		}
	}

	oldClaim := "- live proof (main, not static): PID 63806 serves /api/prompts 200 and /ui/prompts with ADD PROMPT. Static evidence only unless stated."
	if got := simQ4LiveProofHits(oldClaim); len(got) != 5 {
		t.Fatalf("checker missed old live-proof markers (%d): %v", len(got), got)
	}
	cleaned := "Scope: read-only. One binary artifact + its Go source tree. Static evidence only.\n" +
		"| 20 | Supervising process | KeepAlive+RunAtLoad, -dashboard -listen 8787 |\n" +
		"1. Binary's build metadata names parent with vcs.modified=true."
	if got := simQ4LiveProofHits(cleaned); len(got) != 0 {
		t.Fatalf("checker rejected cleaned report: %v", got)
	}
}

func simQ4Get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(b)
}
