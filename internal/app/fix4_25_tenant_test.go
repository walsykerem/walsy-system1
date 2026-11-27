package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The supported isolation boundary is the OS user (HOME). Two sessions with
// different HOMEs must never see each other's config, key, audit or prompts.

func fix425SetenvHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
}

func fix425Paths(t *testing.T) map[string]string {
	t.Helper()
	config, err := soConfigPath()
	if err != nil {
		t.Fatalf("soConfigPath: %v", err)
	}
	dir, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir: %v", err)
	}
	key, err := keyFilePath("TENANT")
	if err != nil {
		t.Fatalf("keyFilePath: %v", err)
	}
	prompt, err := systemPromptPath()
	if err != nil {
		t.Fatalf("systemPromptPath: %v", err)
	}
	prompts, err := promptsPath()
	if err != nil {
		t.Fatalf("promptsPath: %v", err)
	}
	audit, err := auditLogPath()
	if err != nil {
		t.Fatalf("auditLogPath: %v", err)
	}
	return map[string]string{
		"config": config, "state": dir, "key": key,
		"prompt": prompt, "prompts": prompts, "audit": audit,
	}
}

func TestFix425TenantUserHomePathsIsolate(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	fix425SetenvHome(t, a)
	pathsA := fix425Paths(t)
	if err := os.WriteFile(filepath.Join(a, routerDir, "config.txt"), []byte("so_mode=advisory\n"), 0o600); err != nil {
		t.Fatalf("write config A: %v", err)
	}
	modeA := readSOConfig()["so_mode"]

	fix425SetenvHome(t, b)
	pathsB := fix425Paths(t)
	if err := os.MkdirAll(filepath.Join(b, routerDir), 0o700); err != nil {
		t.Fatalf("mkdir state B: %v", err)
	}
	if err := os.WriteFile(filepath.Join(b, routerDir, "config.txt"), []byte("so_mode=enforcing\napprover=tenant-b\n"), 0o600); err != nil {
		t.Fatalf("write config B: %v", err)
	}
	modeB := readSOConfig()

	for name, pa := range pathsA {
		pb := pathsB[name]
		if pb == pa {
			t.Errorf("%s path shared across homes: %s", name, pa)
		}
		if !strings.HasPrefix(pa, a+string(os.PathSeparator)) {
			t.Errorf("%s path escaped home A: %s", name, pa)
		}
		if !strings.HasPrefix(pb, b+string(os.PathSeparator)) {
			t.Errorf("%s path escaped home B: %s", name, pb)
		}
	}
	if modeA != soModeAdvisory {
		t.Errorf("config A not read from HOME A: %q", modeA)
	}
	if modeB["so_mode"] != soModeEnforcing || modeB["approver"] != "tenant-b" {
		t.Errorf("config B not read from HOME B: %v", modeB)
	}
}

type fix425Session struct {
	id         string
	home       string
	port       string
	base       string
	provider   string
	key        string
	rootPrompt string
	named      string
	auditTag   string
	cmd        *exec.Cmd
	log        *os.File
}

func fix425MakeHome(t *testing.T, id string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir state %s: %v", id, err)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatalf("write %s %s: %v", id, name, err)
		}
	}
	write("config.txt", "so_mode=advisory\n")
	write("providers.txt", fmt.Sprintf("TENANT%s,https://tenant%s.example/v1,ENV%s,1\n", id, id, id))
	write("system_prompt.txt", "PROMPT-"+id+"-ROOT")
	write("prompts.json", fmt.Sprintf(`[{"name":"note","text":"PROMPT-%s-NAMED"}]`, id))
	write(routerAuditName, fmt.Sprintf(`{"ts":"t","event":"probe","reason":"AUDIT-%s","provider":"TENANT%s"}`+"\n", id, id))
	keys := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatalf("mkdir keys %s: %v", id, err)
	}
	sum := sha256.Sum256([]byte("TENANT" + id))
	if err := os.WriteFile(filepath.Join(keys, hex.EncodeToString(sum[:8])), []byte("sk-tenant-"+id+"-SECRET"), 0o600); err != nil {
		t.Fatalf("write key %s: %v", id, err)
	}
	return home
}

func fix425FreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// Helper process: a fresh OS process per tenant, so HOME is a real session
// boundary. TestMain overwrites HOME, hence the separate WALSY_FIX425_HOME.
func TestFix425TenantDashboardHelper(t *testing.T) {
	if os.Getenv("WALSY_FIX425_HELPER") != "1" {
		return
	}
	if err := os.Setenv("HOME", os.Getenv("WALSY_FIX425_HOME")); err != nil {
		t.Fatal(err)
	}
	if err := RunDashboard(os.Getenv("WALSY_FIX425_PORT")); err != nil {
		t.Fatal(err)
	}
}

func fix425Spawn(t *testing.T, s *fix425Session) {
	t.Helper()
	logPath := filepath.Join(s.home, "dashboard.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log %s: %v", s.id, err)
	}
	s.log = f
	s.cmd = exec.Command(os.Args[0], "-test.run=^TestFix425TenantDashboardHelper$")
	s.cmd.Stderr = f
	s.cmd.Env = append(os.Environ(),
		"WALSY_FIX425_HELPER=1", "WALSY_FIX425_HOME="+s.home, "WALSY_FIX425_PORT="+s.port,
		"TMPDIR="+s.home, "WALSY_SO_MODE=", "CGO_ENABLED=0",
		"NO_PROXY=*", "no_proxy=*")
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("spawn dashboard %s: %v", s.id, err)
	}
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
		_ = f.Close()
	})
}

func fix425WaitHealth(t *testing.T, s *fix425Session) {
	t.Helper()
	client := &http.Client{Timeout: 400 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.base + "/api/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(s.log.Name())
	t.Fatalf("dashboard %s not healthy: %s", s.id, data)
}

func fix425Fetch(client *http.Client, url string, body []byte, out any) ([]byte, error) {
	var resp *http.Response
	var err error
	if body == nil {
		resp, err = client.Get(url)
	} else {
		req, e := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return data, fmt.Errorf("%s status %d: %s", url, resp.StatusCode, data)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return data, fmt.Errorf("%s returned invalid JSON: %s", url, data)
	}
	return data, nil
}

type fix425Snapshot struct {
	id     string
	body   string
	key    string
	err    error
	prompt struct {
		Prompt string `json:"prompt"`
	}
	prompts []Prompt
	logs    []auditRow
}

func fix425Observe(client *http.Client, s *fix425Session, mutate bool) fix425Snapshot {
	out := fix425Snapshot{id: s.id}
	join := func(data []byte) { out.body += string(data) }
	var data []byte
	var err error
	if data, err = fix425Fetch(client, s.base+"/api/system-prompt", nil, &out.prompt); err != nil {
		out.err = err
		return out
	}
	join(data)
	if data, err = fix425Fetch(client, s.base+"/api/prompts", nil, &out.prompts); err != nil {
		out.err = err
		return out
	}
	join(data)
	if data, err = fix425Fetch(client, s.base+"/api/logs", nil, &out.logs); err != nil {
		out.err = err
		return out
	}
	join(data)
	if mutate {
		body := fmt.Sprintf(`{"name":"extra-%s","text":"PROMPT-%s-EXTRA"}`, s.id, s.id)
		if data, err = fix425Fetch(client, s.base+"/api/prompts", []byte(body), nil); err != nil {
			out.err = err
			return out
		}
		join(data)
		out.prompts = nil
		if data, err = fix425Fetch(client, s.base+"/api/prompts", nil, &out.prompts); err != nil {
			out.err = err
			return out
		}
		join(data)
	}
	keyPath := filepath.Join(s.home, routerDir, "keys")
	sum := sha256.Sum256([]byte(s.provider))
	raw, err := os.ReadFile(filepath.Join(keyPath, hex.EncodeToString(sum[:8])))
	if err != nil {
		out.err = err
		return out
	}
	out.key = string(raw)
	return out
}

func TestFix425TenantConcurrentDashboardSessionsIsolate(t *testing.T) {
	sessions := []*fix425Session{
		{id: "A", home: fix425MakeHome(t, "A"), port: fix425FreePort(t)},
		{id: "B", home: fix425MakeHome(t, "B"), port: fix425FreePort(t)},
	}
	for _, s := range sessions {
		s.base = "http://127.0.0.1:" + s.port
		s.provider = "TENANT" + s.id
		s.key = "sk-tenant-" + s.id + "-SECRET"
		s.rootPrompt = "PROMPT-" + s.id + "-ROOT"
		s.named = "PROMPT-" + s.id + "-NAMED"
		s.auditTag = "AUDIT-" + s.id
		fix425Spawn(t, s)
	}
	for _, s := range sessions {
		fix425WaitHealth(t, s)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	results := make(chan fix425Snapshot, len(sessions))
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *fix425Session) {
			defer wg.Done()
			results <- fix425Observe(client, s, true)
		}(s)
	}
	wg.Wait()
	close(results)

	snaps := make(map[string]fix425Snapshot, len(sessions))
	for snap := range results {
		snaps[snap.id] = snap
	}
	for _, s := range sessions {
		snap := snaps[s.id]
		if snap.err != nil {
			t.Errorf("session %s observe: %v", s.id, snap.err)
			continue
		}
		other := sessions[0]
		if other.id == s.id {
			other = sessions[1]
		}
		if snap.prompt.Prompt != s.rootPrompt {
			t.Errorf("session %s system prompt: got %q", s.id, snap.prompt.Prompt)
		}
		foundNamed, foundExtra := false, false
		for _, p := range snap.prompts {
			if p.Text == s.named {
				foundNamed = true
			}
			if p.Text == "PROMPT-"+s.id+"-EXTRA" {
				foundExtra = true
			}
		}
		if !foundNamed || !foundExtra {
			t.Errorf("session %s prompts missing own entries: %+v", s.id, snap.prompts)
		}
		foundAudit := false
		for _, row := range snap.logs {
			if row.Reason == s.auditTag {
				foundAudit = true
			}
		}
		if !foundAudit {
			t.Errorf("session %s audit missing own tag: %+v", s.id, snap.logs)
		}
		if snap.key != s.key {
			t.Errorf("session %s key changed: %q", s.id, snap.key)
		}
		for _, bad := range []string{
			other.rootPrompt, other.named, other.auditTag, other.key,
			"PROMPT-" + other.id + "-EXTRA",
		} {
			if strings.Contains(snap.body, bad) {
				t.Errorf("session %s leaked tenant %s value %q", s.id, other.id, bad)
			}
		}
	}
}
