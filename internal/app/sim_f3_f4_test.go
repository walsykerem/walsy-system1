package app

import (
	"bufio"
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
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests exercise the shipped binary end to end. Static inspection of the
// artifact cannot prove runtime behavior, so every assertion below reads a
// frame, receipt, or audit line produced by a real subprocess.

const w02_FullCaps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func w02_Caps(state string) string {
	return `"state":` + w02_JSONString(state) + `,` + w02_FullCaps
}

func w02_JSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func w02_SOStub(t *testing.T, posts *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var req struct {
			Questions map[string]struct {
				Criteria map[string]any `json:"criteria"`
			} `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		labels := make([]string, 0, len(req.Questions["gate"].Criteria))
		for label := range req.Questions["gate"].Criteria {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		choice := ""
		for _, label := range labels {
			if label == "yes" {
				choice = label
				break
			}
		}
		if choice == "" && len(labels) > 0 {
			choice = labels[0]
		}
		if choice == "" {
			choice = "yes"
		}
		probs := map[string]float64{}
		for _, label := range labels {
			probs[label] = 0.02
		}
		probs[choice] = 0.95
		answer, _ := json.Marshal(map[string]any{
			"model": "clef",
			"answers": map[string]any{"gate": map[string]any{
				"type": "choice", "choice": choice, "confidence": 0.95, "probabilities": probs,
			}},
			"usage": map[string]any{},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func w02_BuildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "walsy-system1")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/walsy-system1")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func w02_WriteConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "so_mode=advisory\nneed_label=true\nno_scratch=true\nsafe=true\nreversible=true\n" +
		"authority=true\ncalibrated=true\nopt_in=true\nphase=before-model\n"
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func w02_ChildEnv(home, endpoint string) []string {
	drop := map[string]bool{
		"HOME": true, "WALSY_SO_MODE": true,
		"WALSY_SO_ENDPOINT": true, "EVREN_API_KEY": true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if !drop[key] {
			env = append(env, kv)
		}
	}
	return append(env,
		"HOME="+home,
		"WALSY_SO_MODE=",
		"WALSY_SO_ENDPOINT="+endpoint,
		"EVREN_API_KEY=sim-test-key",
	)
}

type w02_Proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cancel context.CancelFunc
}

func w02_Start(t *testing.T, bin, home, endpoint string, args ...string) *w02_Proc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = w02_ChildEnv(home, endpoint)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	return &w02_Proc{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(pipe), cancel: cancel}
}

// w02_Ask writes one JSON-RPC frame and reads exactly one reply line. The tools
// transport replies synchronously, so the SO post delta belongs to this frame.
func (p *w02_Proc) w02_Ask(t *testing.T, frame string) map[string]any {
	t.Helper()
	if _, err := io.WriteString(p.stdin, frame+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := p.stdout.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read reply for %s: %v", frame, err)
	}
	var reply map[string]any
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("reply is not JSON: %s: %v", line, err)
	}
	return reply
}

func (p *w02_Proc) w02_Finish(t *testing.T) {
	t.Helper()
	_ = p.stdin.Close()
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("child exit: %v", err)
	}
	p.cancel()
}

func w02_AuditText(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return string(b)
}

func w02_NoBlockingError(t *testing.T, reply map[string]any, frame string) {
	t.Helper()
	if e, ok := reply["error"].(map[string]any); ok {
		t.Errorf("advisory blocked frame %s: %v", frame, e)
	}
}

// w02_ReplyText unwraps the MCP text content frame; the payload is itself JSON.
func w02_ReplyText(t *testing.T, reply map[string]any) string {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no result: %v", reply)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("reply has no content: %v", reply)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] is not an object: %v", content[0])
	}
	text, _ := first["text"].(string)
	return text
}

// TestW02_SimF3 proves, on a real subprocess, that scope is bound to the JSON-RPC
// method, out-of-scope frames never call SO, advisory never blocks, each
// in-scope frame uses one decision hub, and the shadow verdict is labeled.
func TestW02_SimF3(t *testing.T) {
	posts := &atomic.Int64{}
	srv := w02_SOStub(t, posts)
	bin := w02_BuildBinary(t)

	t.Run("tools hub one decision per frame", func(t *testing.T) {
		home := t.TempDir()
		w02_WriteConfig(t, home)
		p := w02_Start(t, bin, home, srv.URL, "-transport", "tools")

		type step struct {
			name   string
			frame  string
			wantSO int64
		}
		steps := []step{
			{"tools/list out of scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, w02_Caps("sim-f3-list")), 0},
			{"ping out of scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"ping","params":{%s}}`, w02_Caps("sim-f3-ping")), 0},
			{"initialize in scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{%s}}`, w02_Caps("sim-f3-init")), 1},
			{"status in scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"system1_status","arguments":{"state":"sim-f3-status"},%s}}`, w02_FullCaps), 1},
			{"audit in scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"system1_audit","arguments":{"limit":20,"state":"sim-f3-audit"},%s}}`, w02_FullCaps), 1},
			{"decide in scope", fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"system1_decide","arguments":{"item":"intake.intent","state":"sim-f3-decide"},%s}}`, w02_FullCaps), 1},
		}

		var statusReply, auditReply, decideReply string
		for i, s := range steps {
			before := posts.Load()
			reply := p.w02_Ask(t, s.frame)
			delta := posts.Load() - before
			if delta != s.wantSO {
				t.Errorf("%s: SO posts = %d, want %d", s.name, delta, s.wantSO)
			}
			w02_NoBlockingError(t, reply, s.frame)
			switch i {
			case 3:
				statusReply = w02_ReplyText(t, reply)
			case 4:
				auditReply = w02_ReplyText(t, reply)
			case 5:
				decideReply = w02_ReplyText(t, reply)
			}
		}
		p.w02_Finish(t)

		if !strings.Contains(statusReply, `"providers"`) {
			t.Errorf("system1_status reply lacks providers: %s", statusReply)
		}
		if !strings.Contains(auditReply, `"source":"preflight"`) {
			t.Errorf("system1_audit reply lacks advisory shadow label: %s", auditReply)
		}
		if !strings.Contains(decideReply, `"item":"intake.intent"`) {
			t.Errorf("system1_decide reply lacks the item: %s", decideReply)
		}
		audit := w02_AuditText(t, home)
		if !strings.Contains(audit, `"source":"preflight"`) {
			t.Error("audit log lacks source=preflight shadow verdict")
		}
		if !strings.Contains(audit, `"method":"systemone"`) {
			t.Error("audit log lacks the SO call record")
		}
	})

	t.Run("stdio passthrough and advisory shadow", func(t *testing.T) {
		home := t.TempDir()
		w02_WriteConfig(t, home)
		before := posts.Load()

		frames := []string{
			fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{%s}}`, w02_Caps("sim-f3-stdio-notify")),
			fmt.Sprintf(`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{%s}}`, w02_Caps("sim-f3-stdio-list")),
			fmt.Sprintf(`{"jsonrpc":"2.0","id":12,"method":"initialize","params":{%s}}`, w02_Caps("sim-f3-stdio-init")),
			fmt.Sprintf(`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/sim-f3.txt"},%s}}`, w02_FullCaps),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "/bin/cat")
		cmd.Env = w02_ChildEnv(home, srv.URL)
		cmd.Stdin = strings.NewReader(strings.Join(frames, "\n") + "\n")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("stdio child: %v\n%s", err, stderr.String())
		}
		got := strings.FieldsFunc(stdout.String(), func(r rune) bool { return r == '\n' || r == '\r' })
		if len(got) != len(frames) {
			t.Fatalf("forwarded %d frames, want %d: %s", len(got), len(frames), stdout.String())
		}
		for i := range frames {
			if got[i] != frames[i] {
				t.Errorf("frame %d changed on the wire:\n got %s\nwant %s", i, got[i], frames[i])
			}
			if strings.Contains(got[i], "-32001") {
				t.Errorf("frame %d was blocked by advisory: %s", i, got[i])
			}
		}
		if delta := posts.Load() - before; delta != 2 {
			t.Errorf("stdio SO posts = %d, want 2 (initialize and tools/call only)", delta)
		}
		audit := w02_AuditText(t, home)
		if !strings.Contains(audit, `"source":"preflight"`) {
			t.Error("stdio audit lacks source=preflight shadow verdict")
		}
	})
}

// TestW02_SimF4 verifies the observable receipt chain produced by real decisions:
// one receipt per request, sequential seq, fresh random nonce, link to previous
// hash, response_id none, and the source=so label required by the manifest.
func TestW02_SimF4(t *testing.T) {
	posts := &atomic.Int64{}
	srv := w02_SOStub(t, posts)
	bin := w02_BuildBinary(t)
	home := t.TempDir()
	w02_WriteConfig(t, home)

	p := w02_Start(t, bin, home, srv.URL, "-transport", "tools")
	frames := []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":21,"method":"initialize","params":{%s}}`, w02_Caps("sim-f4-init")),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"system1_status","arguments":{"state":"sim-f4-a"},%s}}`, w02_FullCaps),
		fmt.Sprintf(`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"system1_status","arguments":{"state":"sim-f4-b"},%s}}`, w02_FullCaps),
	}
	for _, f := range frames {
		reply := p.w02_Ask(t, f)
		w02_NoBlockingError(t, reply, f)
	}
	p.w02_Finish(t)

	path := filepath.Join(home, ".walsy-system1", "receipt.chain")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read receipt.chain: %v", err)
	}
	lines := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) != len(frames) {
		t.Fatalf("receipts = %d, want %d", len(lines), len(frames))
	}

	type row struct {
		Seq        int    `json:"seq"`
		Nonce      string `json:"nonce"`
		Prev       string `json:"prev"`
		Decision   string `json:"decision"`
		Reason     string `json:"reason"`
		Method     string `json:"method"`
		ResponseID string `json:"response_id"`
	}
	seenNonce := map[string]bool{}
	var head []byte
	for i, line := range lines {
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("receipt %d: %v", i, err)
		}
		if r.Seq != i+1 {
			t.Errorf("receipt %d seq = %d, want %d", i, r.Seq, i+1)
		}
		if len(r.Nonce) != 32 {
			t.Errorf("receipt %d nonce = %q, want 32 hex chars", i, r.Nonce)
		}
		if _, err := hex.DecodeString(r.Nonce); err != nil {
			t.Errorf("receipt %d nonce is not hex: %v", i, err)
		}
		if seenNonce[r.Nonce] {
			t.Errorf("receipt %d reuses nonce %s", i, r.Nonce)
		}
		seenNonce[r.Nonce] = true
		if r.ResponseID != "none" {
			t.Errorf("receipt %d response_id = %q, want none", i, r.ResponseID)
		}
		if r.Prev != hex.EncodeToString(head) {
			t.Errorf("receipt %d prev = %q, want %q (hash chain broken)", i, r.Prev, hex.EncodeToString(head))
		}
		if r.Decision == "" || r.Reason == "" || r.Method == "" {
			t.Errorf("receipt %d lacks a routing fact: %s", i, line)
		}
		sum := sha256.Sum256(append(append([]byte{}, head...), []byte(line)...))
		head = sum[:]
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("receipt.chain mode = %o, want 600", st.Mode().Perm())
	}

	all := strings.Join(lines, "\n") + "\n" + w02_AuditText(t, home)
	if !strings.Contains(all, `"source":"so"`) {
		t.Error(`F4 evidence gap: no receipt or audit line carries "source":"so"; receipt chain itself is valid`)
	}
	if posts.Load() != int64(len(frames)) {
		t.Errorf("SO posts = %d, want %d", posts.Load(), len(frames))
	}
}
