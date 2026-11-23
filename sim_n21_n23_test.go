package main

// Hermetic behavioural simulations for MANIFEST N21 and N23.
// Real production transport, handler, bridge and cache code are driven with a
// local httptest SO stub and a temp HOME. No real SO call, no real secret, no
// production or existing-test edit, no process restart.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const w13_Caps = `"need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"opt_in":true,"phase":"before-model"`

func w13_Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "")
	routerReset()
	t.Cleanup(routerReset)
	return home
}

// w13_SO replaces the real endpoint with a stub. Every POST signals posts
// so a test can observe how many decisions crossed the SO boundary.
func w13_SO(t *testing.T, answer string, posts chan<- struct{}) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts != nil {
			select {
			case posts <- struct{}{}:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	orig := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() {
		soEndpoint = orig
		srv.Close()
	})
}

func w13_Answer(choice string, conf float64) string {
	b, err := json.Marshal(map[string]any{
		"model": "clef",
		"answers": map[string]any{
			"gate": map[string]any{
				"type":       "choice",
				"choice":     choice,
				"confidence": conf,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func w13_Prompts(name, text string) []Prompt {
	return []Prompt{{Name: name, Text: text}}
}

func w13_WaitPost(t *testing.T, posts <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-posts:
	case <-time.After(3 * time.Second):
		t.Fatalf("no SO POST observed within 3s (%s)", what)
	}
}

func w13_Post(t *testing.T, url string, body []byte) []byte {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return out
}

// TestSimN21 simulates a server response frame arriving with no client frame.
// Any decision work or receipt in that window belongs to the outbound
// (server -> client) direction. MANIFEST N21 wants a wait-capable second gate or
// at least a non-blocking outbound receipt. This test checks observable
// effects, not internal names.
func TestSimN21(t *testing.T) {
	home := w13_Home(t)
	t.Setenv("EVREN_API_KEY", "sim-n21-key") // dummy, never read from disk
	posts := make(chan struct{}, 8)
	w13_SO(t, w13_Answer("yes", 0.97), posts)
	t.Setenv("WALSY_SO_MODE", "enforcing")

	serverFrame := append([]byte(`{"jsonrpc":"2.0","id":42,"result":{"content":[{"type":"text","text":"upstream ok"}]}}`), '\n')
	var toServer, toClient bytes.Buffer
	bridgeStdio(bytes.NewReader(nil), &toClient, &toServer, bytes.NewReader(serverFrame))

	// The response itself must still reach the client (non-blocking).
	if !bytes.Equal(toClient.Bytes(), serverFrame) {
		t.Fatalf("outbound response not relayed: got %q want %q", toClient.Bytes(), serverFrame)
	}

	soPosts := len(posts)
	receipts := 0
	outboundReceipt := false
	if b, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain")); err == nil {
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			receipts++
			var m map[string]any
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			for _, v := range m {
				s, _ := v.(string)
				l := strings.ToLower(s)
				if strings.Contains(l, "outbound") || strings.Contains(l, "egress") || strings.Contains(l, "response") {
					outboundReceipt = true
				}
			}
		}
	}

	// relay_frame audit (phase=server) is not a gate decision: require a
	// decision/gate/wait marker in the same line before counting it.
	outboundAudit := false
	if b, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName)); err == nil {
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			l := strings.ToLower(string(line))
			gateish := strings.Contains(l, `"event":"decision`) || strings.Contains(l, "wait") || strings.Contains(l, "gate")
			direction := strings.Contains(l, "outbound") || strings.Contains(l, "egress") || strings.Contains(l, "response")
			if gateish && direction {
				outboundAudit = true
			}
		}
	}

	if soPosts == 0 && !outboundReceipt && !outboundAudit {
		t.Fatalf("N21 BROKEN: outbound side has no decision point and no receipt "+
			"(so_posts=%d receipts=%d outbound_receipt=%v outbound_audit=%v); "+
			"inspectServer currently rewrites only initialize results and returns every other frame unchanged",
			soPosts, receipts, outboundReceipt, outboundAudit)
	}
}

// TestSimN23 simulates identical requests crossing a prompt-set change.
// MANIFEST N23 wants the active prompt-set hash folded into gateKey, so two
// otherwise identical frames must not share a cache cell across that change.
func TestSimN23(t *testing.T) {
	w13_Home(t)
	req := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"n23-tool","arguments":{},"state":"decide: n23 prompt binding",` + w13_Caps + `}}`)
	cap := soCapability()

	setA := w13_Prompts("alpha", "review seven checks")
	setB := w13_Prompts("alpha", "review eight checks")
	setC := w13_Prompts("beta", "review eight checks")
	setD := []Prompt{{Name: "alpha", Text: "review eight checks"}, {Name: "beta", Text: "extra"}}

	save := func(ps []Prompt) [32]byte {
		t.Helper()
		if err := savePrompts(ps); err != nil {
			t.Fatalf("savePrompts: %v", err)
		}
		return gateKeyBytes(req, cap)
	}

	kEmpty := gateKeyBytes(req, cap)
	kA := save(setA)
	if kA == kEmpty {
		t.Fatal("N23 BROKEN: adding a named prompt did not change gateKey")
	}
	if kA2 := gateKeyBytes(req, cap); kA2 != kA {
		t.Fatal("N23: gateKey not deterministic for same prompt set")
	}
	kB := save(setB)
	if kB == kA {
		t.Fatal("N23 BROKEN: changing prompt text did not change gateKey")
	}
	kC := save(setC)
	if kC == kB {
		t.Fatal("N23 BROKEN: changing prompt name did not change gateKey")
	}
	kD := save(setD)
	if kD == kC {
		t.Fatal("N23 BROKEN: adding a prompt did not change gateKey")
	}

	// The hash must reach the real cache key, not only gateKeyBytes.
	d := &decisionBridge{cache: newShardedCache(), oraclePin: []byte("sim-pin")}
	save(setA)
	keyA := d.cacheKey(req, cap, 3)
	d.cache.store(keyA, cachedDecision{Choice: "yes", Reason: "margin=1"})
	save(setB)
	keyB := d.cacheKey(req, cap, 3)
	if _, hit := d.cache.lookup(keyB); hit {
		t.Fatal("N23 BROKEN: decision cache crossed a prompt-set text change")
	}
	save(setA)
	if got, hit := d.cache.lookup(d.cacheKey(req, cap, 3)); !hit || got.Choice != "yes" {
		t.Fatalf("N23: cache must be deterministic and reusable within one prompt set (hit=%v got=%+v)", hit, got)
	}

	// End-to-end through the real HTTP handler. Identical bytes, different
	// active prompt sets: each must reach SO. Enforcing makes decide() inline so
	// the first cache store finishes before the next request is sent.
	t.Setenv("EVREN_API_KEY", "sim-n23-key") // dummy, never read from disk
	t.Setenv("WALSY_SO_MODE", "enforcing")
	posts := make(chan struct{}, 8)
	w13_SO(t, w13_Answer("yes", 0.95), posts)
	srv := httptest.NewServer(mcpHTTPHandler())
	defer srv.Close()

	frame := append([]byte(`{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2025-06-18",`+w13_Caps+`}}`), '\n')
	save(setA)
	body := w13_Post(t, srv.URL, frame)
	if !bytes.Contains(body, []byte(`"jsonrpc"`)) {
		t.Fatalf("N23: unexpected HTTP response %q", body)
	}
	w13_WaitPost(t, posts, "first request / prompt set A")

	save(setB)
	body = w13_Post(t, srv.URL, frame)
	if !bytes.Contains(body, []byte(`"jsonrpc"`)) {
		t.Fatalf("N23: unexpected HTTP response after prompt change %q", body)
	}
	w13_WaitPost(t, posts, "identical request / prompt set B")
}
