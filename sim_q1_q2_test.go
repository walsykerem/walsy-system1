package main

// Genuine simulation for MANIFEST items Q1 and Q2 (batch B21, report quality).
// Q1: RAPOR.md must carry the heading "static map; advisory, fail-open by
// default", and that claim must hold at runtime: with empty WALSY_SO_MODE the
// real HTTP handler forwards a frame that enforcing mode refuses, even when the
// SO stub answers "no" or returns 500.
// Q2: RAPOR.md must not carry a verbatim digest JSON line (it was truncated at
// 200 chars); the full digest belongs in the sibling .digest.json file.
// No production file and no existing test is edited. Fake key only: no real key
// is read and no outbound SO call is made.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	w21_Heading    = "static map; advisory, fail-open by default"
	w21_ReportDflt = "/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/walsy-arch-map/RAPOR.md"
	// Out-of-scope destructive shell call: enforcingGate refuses it, advisory forwards.
	w21_Frame    = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"run","arguments":{"command":"rm -rf /tmp/simq"}}}`
	w21_Upstream = `{"jsonrpc":"2.0","id":7,"result":{"simq":"upstream-echo"}}`
	w21_NoBody   = `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.99}},"usage":{"evren":{"credits":1}}}`
)

// simShippedBinary resolves the binary from the package directory, not from a
// baked absolute path.
func simShippedBinary(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("walsy-system1")
	if err != nil {
		t.Fatalf("resolve shipped binary path: %v", err)
	}
	return p
}

// simMachOBinary selects the Mach-O artifact the static Q3/Q5 checks parse.
// Native darwin uses the shipped binary; Linux CI cross-builds one and names it
// here so the same Mach-O assertions still run.
func simMachOBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WALSY_SIM_MACHO_BINARY"); p != "" {
		return p
	}
	return simShippedBinary(t)
}

// simMachODigest selects the digest describing the Mach-O artifact above.
func simMachODigest() string {
	if p := os.Getenv("WALSY_SIM_MACHO_DIGEST"); p != "" {
		return p
	}
	return "walsy-system1.digest.json"
}

type simDigestTarget struct {
	ContractVersion int `json:"contract_version"`
	Target          struct {
		Size       int64  `json:"size"`
		FileSHA256 string `json:"file_sha256"`
		HashScope  string `json:"hash_scope"`
	} `json:"target"`
}

// simReadDigest uses the parent-owned digest fixture as the SHA source; tests
// must not bake the binary hash into their own source.
func simReadDigest(t *testing.T, path string) ([]byte, simDigestTarget) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read digest %s: %v", path, err)
	}
	var d simDigestTarget
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse digest %s: %v", path, err)
	}
	sha := d.Target.FileSHA256
	if len(sha) != 64 || strings.ToLower(sha) != sha {
		t.Fatalf("digest %s: target.file_sha256 must be 64 lowercase hex chars, got %q", path, sha)
	}
	if _, err := hex.DecodeString(sha); err != nil {
		t.Fatalf("digest %s: target.file_sha256 is not hex: %v", path, err)
	}
	return raw, d
}

func w21_ReportPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WALSY_RAPOR"); p != "" {
		return p
	}
	for _, p := range []string{w21_ReportDflt, filepath.Join("..", "walsy-arch-map", "RAPOR.md"), "RAPOR.md"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	t.Fatal("RAPOR.md not found; set WALSY_RAPOR")
	return ""
}

func w21_Env(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "") // empty: config must fail open to advisory
	t.Setenv("EVREN_API_KEY", "sim-q-fake-key")
	routerReset()
	t.Cleanup(routerReset)
}

func w21_SO(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	old := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = old; srv.Close() })
	return srv
}

// w21_UpstreamServer is the real upstream; it records the exact frame bytes it got.
func w21_UpstreamServer(t *testing.T) (*httptest.Server, chan string) {
	t.Helper()
	seen := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- string(b)
		_, _ = io.WriteString(w, w21_Upstream)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// w21_Call drives the real handler over a real loopback HTTP transport.
func w21_Call(t *testing.T, h http.Handler, frame string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(frame))
	if err != nil {
		t.Fatalf("POST to MCP handler: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read MCP response: %v", err)
	}
	return resp.StatusCode, string(b)
}

// Test_w21_Q1_HeadingAndFailOpen proves both the text and the truth of Q1.
func Test_w21_Q1_HeadingAndFailOpen(t *testing.T) {
	raw, err := os.ReadFile(w21_ReportPath(t))
	if err != nil {
		t.Fatalf("read RAPOR.md: %v", err)
	}
	head := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			head = line
			break
		}
	}
	if !strings.Contains(head, w21_Heading) {
		t.Fatalf("Q1 heading missing: first line = %.160q, want substring %q", head, w21_Heading)
	}

	w21_Env(t)
	if got := soMode(); got != soModeAdvisory {
		t.Fatalf("Q1 claim false: default soMode = %q, want advisory", got)
	}

	up, upSeen := w21_UpstreamServer(t)
	var soHits int32
	w21_SO(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&soHits, 1)
		_, _ = io.WriteString(w, w21_NoBody)
	})
	h := mcpHTTPHandlerUpstream(up.URL)
	code, body := w21_Call(t, h, w21_Frame)
	if code != http.StatusOK || strings.TrimSpace(body) != w21_Upstream {
		t.Fatalf("advisory default blocked/changed frame: code=%d body=%q", code, body)
	}
	select {
	case got := <-upSeen:
		if got != w21_Frame {
			t.Fatalf("upstream received changed frame: %q", got)
		}
	default:
		t.Fatal("upstream never received the frame")
	}
	// A deny verdict is advisory only; the frame still reached the upstream.
	t.Logf("advisory SO shadow POST count: %d (scope-dependent, not a gate)", atomic.LoadInt32(&soHits))

	// Enforcing contrast: the same frame must be refused. Proves the forward
	// above is a real advisory behavior, not a dead handler.
	t.Run("enforcing-contrast", func(t *testing.T) {
		w21_Env(t)
		t.Setenv("WALSY_SO_MODE", "enforcing") // deliberate contrast; default case above stays empty
		up2, upSeen2 := w21_UpstreamServer(t)
		w21_SO(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, w21_NoBody)
		})
		code2, body2 := w21_Call(t, mcpHTTPHandlerUpstream(up2.URL), w21_Frame)
		if !strings.Contains(body2, "-32001") {
			t.Fatalf("enforcing did not refuse frame: code=%d body=%q", code2, body2)
		}
		select {
		case <-upSeen2:
			t.Fatal("enforcing forwarded a refused frame upstream")
		default:
		}
	})

	// Fail open: an erroring oracle still cannot block or 503 the client.
	t.Run("oracle-500-fails-open", func(t *testing.T) {
		w21_Env(t)
		up3, upSeen3 := w21_UpstreamServer(t)
		w21_SO(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "oracle down", http.StatusInternalServerError)
		})
		code3, body3 := w21_Call(t, mcpHTTPHandlerUpstream(up3.URL), w21_Frame)
		if code3 != http.StatusOK || strings.TrimSpace(body3) != w21_Upstream {
			t.Fatalf("500 oracle blocked/changed frame: code=%d body=%q", code3, body3)
		}
		select {
		case got := <-upSeen3:
			if got != w21_Frame {
				t.Fatalf("upstream received changed frame after 500: %q", got)
			}
		default:
			t.Fatal("upstream never received the frame after 500")
		}
	})
}

// Test_w21_Q2_NoVerbatimDigest checks the deletion and proves the full digest was
// preserved in the sibling file rather than lost from the report set.
func Test_w21_Q2_NoVerbatimDigest(t *testing.T) {
	path := w21_ReportPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read RAPOR.md: %v", err)
	}
	markers := []string{`"file_sha256"`, `"contract_version"`, `"gitoid"`, `"hash_scope"`}
	for i, line := range strings.Split(string(raw), "\n") {
		for _, m := range markers {
			if strings.Contains(line, m) {
				t.Errorf("Q2: %s:%d still carries verbatim digest JSON (%d chars, marker %s)", path, i+1, len(line), m)
				break
			}
		}
	}
	if !strings.Contains(string(raw), "walsy-system1.digest.json") {
		t.Error("Q2: report no longer names the sibling digest file")
	}

	sib := filepath.Join(filepath.Dir(path), "walsy-system1.digest.json")
	sraw, dj := simReadDigest(t, sib)
	if len(sraw) <= 200 || !strings.Contains(string(sraw), `"file_sha256"`) || !strings.Contains(string(sraw), `"contract_version"`) {
		t.Fatalf("Q2: sibling digest is not a full JSON digest (%d bytes)", len(sraw))
	}

	// Strict: the preserved digest must actually describe the shipped binary.
	// Q2 deletes the truncated copy from RAPOR.md, so the sibling is now the
	// only digest record; a stale sibling silently misidentifies the binary.
	b, err := os.ReadFile(simShippedBinary(t))
	if err != nil {
		t.Fatalf("Q2: shipped binary unreadable: %v", err)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	if dj.Target.FileSHA256 != got {
		t.Errorf("Q2: sibling digest stale: target.file_sha256=%s, sha256(binary)=%s; fix %s:1 by regenerating from the current binary", dj.Target.FileSHA256, got, sib)
	}
	if dj.Target.Size != int64(len(b)) {
		t.Errorf("Q2: sibling digest target.size=%d, binary size=%d", dj.Target.Size, len(b))
	}
	if dj.ContractVersion != 1 || dj.Target.HashScope != "whole-file" {
		t.Errorf("Q2: sibling digest metadata unexpected: contract_version=%d hash_scope=%q", dj.ContractVersion, dj.Target.HashScope)
	}
}
