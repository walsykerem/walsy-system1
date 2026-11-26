package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditLines(t *testing.T) []string {
	t.Helper()
	home := os.Getenv("HOME")
	data, err := os.ReadFile(filepath.Join(home, ".walsy-system1", "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestRelayLogRecordsOutcomeWithoutPayload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	relayLog("client", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"key":"sk-secret"}}}`))
	relayLog("server", []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no method sk-secret"}}`))
	relayLog("client", []byte(`{"broken"`))
	lines := auditLines(t)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3: %v", len(lines), lines)
	}
	for i, want := range []string{`"mode":"tools/call"`, `"status":"error_-32601"`, `"status":"malformed"`} {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d = %s, want %s", i, lines[i], want)
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "sk-secret") {
			t.Fatalf("payload leaked into audit: %s", line)
		}
	}
}

func TestBridgeLogsBothDirections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clientIn := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var clientOut, upstreamIn bytes.Buffer
	upstreamOut := strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}` + "\n")
	bridgeStdio(clientIn, &clientOut, &upstreamIn, upstreamOut)
	if !strings.Contains(clientOut.String(), "protocolVersion") {
		t.Fatalf("response not forwarded: %s", clientOut.String())
	}
	joined := strings.Join(auditLines(t), "\n")
	if !strings.Contains(joined, `"phase":"client"`) || !strings.Contains(joined, `"phase":"server"`) {
		t.Fatalf("audit missing directions: %s", joined)
	}
}

func TestAuditRotatesAtCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("HOME"), ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), routerAuditMaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	relayLog("client", []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != routerAuditMaxBytes+1 {
		t.Fatalf("rotated file: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() > 512 {
		t.Fatalf("fresh audit after rotation: %v", err)
	}
}
