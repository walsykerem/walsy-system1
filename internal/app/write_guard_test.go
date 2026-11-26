package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// audCFrame builds a real MCP tools/call frame; id nil means a notification.
func audCFrame(t *testing.T, id any, tool, args string) []byte {
	t.Helper()
	f := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": json.RawMessage(args)},
	}
	if id != nil {
		f["id"] = id
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// audCEnv gives one case a clean HOME, an offline SO stub and a reset router,
// so the guard verdict is the only thing that can block the frame.
func audCEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "64")
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "32")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	}))
	oldEndpoint, oldClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	t.Cleanup(func() {
		soEndpoint, soClient = oldEndpoint, oldClient
		srv.Close()
	})
	routerReset()
	return home
}

func audCAudit(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	return string(b)
}

// The write guard is a refusal the caller sees on the wire, so every case goes
// through the real enforcing gate of a bridge and checks the -32001 body, the
// silent notification path and the reason code in audit.jsonl.
func TestAudCWriteGuardOnTheWire(t *testing.T) {
	const base = "/tmp/audc"
	cases := []struct {
		name      string
		send      func(t *testing.T, d *decisionBridge) ([]byte, bool)
		wantBlock bool
		wantWire  []string
		wantNone  bool
		audit     []string
	}{
		{
			name: "unread write refused",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				return d.enforcingGate(audCFrame(t, 1, "write_file",
					`{"path":"`+base+`/unread.txt","content":"x"}`))
			},
			wantBlock: true,
			wantWire: []string{`"code":-32001`,
				"blocked: write to a path this session never touched: " + base + "/unread.txt"},
			audit: []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "read licenses same write",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				if out, blocked := d.enforcingGate(audCFrame(t, 1, "read_file",
					`{"path":"`+base+`/same.txt"}`)); blocked {
					t.Fatalf("read blocked: %s", out)
				}
				return d.enforcingGate(audCFrame(t, 2, "write_file",
					`{"path":"`+base+`/same.txt","content":"y"}`))
			},
			wantBlock: false,
			audit:     []string{`"event":"gate"`},
		},
		{
			name: "absolute read licenses bare write",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				d.enforcingGate(audCFrame(t, 1, "read_file", `{"path":"`+base+`/dir/a.go"}`))
				return d.enforcingGate(audCFrame(t, 2, "write_file",
					`{"path":"a.go","content":"y"}`))
			},
			wantBlock: false,
		},
		{
			name: "bare read never licenses pathed write",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				d.enforcingGate(audCFrame(t, 1, "read_file", `{"path":"a.go"}`))
				return d.enforcingGate(audCFrame(t, 2, "write_file",
					`{"path":"`+base+`/sub/a.go","content":"y"}`))
			},
			wantBlock: true,
			wantWire:  []string{"never touched: " + base + "/sub/a.go"},
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "rename dest is judged too",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				d.enforcingGate(audCFrame(t, 1, "read_file", `{"path":"`+base+`/src.txt"}`))
				return d.enforcingGate(audCFrame(t, 2, "rename_file",
					`{"path":"`+base+`/src.txt","dest":"`+base+`/dest.txt"}`))
			},
			wantBlock: true,
			wantWire:  []string{"never touched: " + base + "/dest.txt"},
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "write without a path refused",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				return d.enforcingGate(audCFrame(t, 1, "write_file", `{}`))
			},
			wantBlock: true,
			wantWire:  []string{"blocked: write with no readable path"},
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "malformed arguments refused",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				return d.enforcingGate(audCFrame(t, 1, "write_file", `[1,2]`))
			},
			wantBlock: true,
			wantWire:  []string{"blocked: write with no readable path"},
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "notification blocked silently",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				return d.enforcingGate(audCFrame(t, nil, "write_file",
					`{"path":"`+base+`/note.txt","content":"x"}`))
			},
			wantBlock: true,
			wantNone:  true,
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "repeat limit then reset",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				args := `{"path":"` + base + `/rep.txt","content":"same"}`
				d.enforcingGate(audCFrame(t, 1, "read_file", `{"path":"`+base+`/rep.txt"}`))
				for i := 0; i < guardRepeatLimit; i++ {
					if out, blocked := d.enforcingGate(audCFrame(t, i+2, "write_file", args)); blocked {
						t.Fatalf("repeat %d blocked early: %s", i, out)
					}
				}
				out, blocked := d.enforcingGate(audCFrame(t, 20, "write_file", args))
				if !blocked {
					t.Fatal("repeat past the limit was forwarded")
				}
				if _, again := d.enforcingGate(audCFrame(t, 21, "write_file", args)); again {
					t.Fatal("counter did not reset after firing")
				}
				return out, true
			},
			wantBlock: true,
			wantWire:  []string{"repeated identical call 8 times"},
			audit:     []string{`"event":"writeguard"`, `"reason":"redacted"`},
		},
		{
			name: "advisory audits without blocking",
			send: func(t *testing.T, d *decisionBridge) ([]byte, bool) {
				d.advisoryHooks(audCFrame(t, 1, "write_file",
					`{"path":"`+base+`/adv.txt","content":"x"}`))
				return nil, false
			},
			wantBlock: false,
			audit:     []string{`"mode":"advisory"`, `"reason":"unread-write"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := audCEnv(t)
			d := newDecisionBridge()
			out, blocked := c.send(t, d)
			if blocked != c.wantBlock {
				t.Fatalf("blocked = %v, want %v (wire %s)", blocked, c.wantBlock, out)
			}
			if c.wantNone && len(out) != 0 {
				t.Fatalf("notification answered on the wire: %s", out)
			}
			for _, want := range c.wantWire {
				if !strings.Contains(string(out), want) {
					t.Fatalf("wire %s missing %q", out, want)
				}
			}
			if len(c.audit) > 0 {
				log := audCAudit(t, home)
				for _, want := range c.audit {
					if !strings.Contains(log, want) {
						t.Fatalf("audit %s missing %q", log, want)
					}
				}
			}
		})
	}
}
