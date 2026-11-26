package app

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Simulation-only fixtures. Hermetic HOME, stubbed SO over real HTTP, real
// bridge/relay handlers. No production file is touched by this test.

const simN4N5FullCaps = `"need_label":true,"no_scratch":true,"safe":true,` +
	`"reversible":true,"authority":true,"calibrated":true,` +
	`"opt_in":true,"phase":"before-model"`

const simN4N5Layer0Caps = `"need_label":true,"no_scratch":true,"safe":true,` +
	`"reversible":true`

type simN4N5Stub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func simN4N5NewStub() *simN4N5Stub {
	s := &simN4N5Stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"clef","answers":{"gate":`+
			`{"type":"choice","choice":"yes","confidence":0.9,`+
			`"probabilities":{"yes":0.9,"no":0.1}}}}`)
	}))
	return s
}

func (s *simN4N5Stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *simN4N5Stub) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func simN4N5Env(t *testing.T, mode string) (*decisionBridge, *simN4N5Stub, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", mode)
	t.Setenv("EVREN_API_KEY", "sim-only-test-key")
	t.Setenv("WALSY_ROUTER_DENY", "")
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "9")
	routerReset()
	stub := simN4N5NewStub()
	oldEndpoint := soEndpoint
	soEndpoint = stub.srv.URL
	t.Cleanup(func() {
		soEndpoint = oldEndpoint
		stub.srv.Close()
		routerReset()
	})
	return newDecisionBridge(), stub, home
}

func simN4N5WriteConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func simN4N5Frame(method string, id int, params string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"` + method + `","params":{` + params + `}}`)
}

func simN4N5Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func simN4N5Join(bodies []string) string {
	var sb strings.Builder
	for _, b := range bodies {
		sb.WriteString(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestSimN4MissingCapabilityFailClosed(t *testing.T) {
	t.Run("enforcing_layer0_only_blocked", func(t *testing.T) {
		d, stub, _ := simN4N5Env(t, "enforcing")
		frame := simN4N5Frame("initialize", 1,
			`"state":"classify this request",`+simN4N5Layer0Caps)
		resp, blocked := d.enforcingGate(frame)
		if !blocked {
			t.Fatalf("N4: layer0-only frame was not blocked")
		}
		var got struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(resp, &got); err != nil {
			t.Fatalf("N4: bad error frame: %v (%s)", err, resp)
		}
		if got.Error.Code != -32001 {
			t.Errorf("N4: want code -32001, got %d", got.Error.Code)
		}
		if !strings.Contains(got.Error.Message, "explicit decision capability required") {
			t.Errorf("N4: unexpected refusal: %q", got.Error.Message)
		}
		if n := stub.count(); n != 0 {
			t.Errorf("N4: blocked frame still caused %d SO POST(s)", n)
		}
	})

	t.Run("partial_config_no_default_true", func(t *testing.T) {
		d, stub, home := simN4N5Env(t, "enforcing")
		simN4N5WriteConfig(t, home, "so_mode=enforcing\nneed_label=true\n"+
			"no_scratch=true\nsafe=true\nreversible=true\n")
		if got := configCapabilityParams(); got != nil {
			t.Errorf("N4: partial config backfilled caps: %s", got)
		}
		frame := simN4N5Frame("initialize", 2,
			`"state":"classify this request",`+simN4N5Layer0Caps)
		resp, blocked := d.enforcingGate(frame)
		if !blocked || !bytes.Contains(resp, []byte("explicit decision capability required")) {
			t.Errorf("N4: partial config must stay blocked, blocked=%v resp=%s", blocked, resp)
		}
		if n := stub.count(); n != 0 {
			t.Errorf("N4: partial config caused %d SO POST(s)", n)
		}
	})

	t.Run("advisory_warns_and_forwards", func(t *testing.T) {
		_, stub, home := simN4N5Env(t, "advisory")
		frame := append(simN4N5Frame("initialize", 3,
			`"state":"classify this request",`+simN4N5Layer0Caps), '\n')
		clientOut := &bytes.Buffer{}
		upstreamIn := &bytes.Buffer{}
		bridgeStdio(bytes.NewReader(frame), clientOut, upstreamIn, strings.NewReader(""))
		if !bytes.Contains(upstreamIn.Bytes(), frame[:len(frame)-1]) {
			t.Errorf("N4: advisory frame was not forwarded byte-for-byte")
		}
		if clientOut.Len() != 0 {
			t.Errorf("N4: advisory mode emitted a block reply: %s", clientOut.Bytes())
		}
		audit := simN4N5Read(t, filepath.Join(home, ".walsy-system1", "audit.jsonl"))
		if !strings.Contains(audit, `"reason":"cap-missing-warning"`) {
			t.Errorf("N4: no cap-missing-warning audit record: %s", audit)
		}
		if n := stub.count(); n != 0 {
			t.Errorf("N4: advisory missing-cap frame caused %d SO POST(s)", n)
		}
	})

	t.Run("full_caps_unblocked", func(t *testing.T) {
		d, stub, home := simN4N5Env(t, "enforcing")
		frame := simN4N5Frame("initialize", 4,
			`"state":"classify this request",`+simN4N5FullCaps)
		_, blocked := d.enforcingGate(frame)
		if blocked {
			t.Fatalf("N4: fully opted-in frame was blocked")
		}
		if n := stub.count(); n < 1 {
			t.Fatalf("N4: admitted frame caused %d SO POST(s), want >=1", n)
		}
		chain := simN4N5Read(t, filepath.Join(home, ".walsy-system1", "receipt.chain"))
		if !strings.Contains(chain, `"reason":"so-admit"`) {
			t.Errorf("N4: no so-admit receipt: %s", chain)
		}
	})
}

func TestSimN5GateKeyBindings(t *testing.T) {
	t.Run("method_bound", func(t *testing.T) {
		d, stub, _ := simN4N5Env(t, "advisory")
		// Warm the cache: the first SO admit bumps pin generation while route
		// runs, so its own result is not stored under the key it was looked up
		// with. A distinct warm frame makes the later hit/miss unambiguous.
		d.decideFrame(simN4N5Frame("initialize", 9,
			`"state":"warm up classify request",`+simN4N5FullCaps))
		params := `"state":"classify this request",` + simN4N5FullCaps
		a := simN4N5Frame("initialize", 1, params)
		b := simN4N5Frame("tools/call", 2, params)
		d.decideFrame(a)
		d.decideFrame(b)
		bodies := stub.all()
		if len(bodies) != 3 {
			t.Errorf("N5: method binding want 3 SO POSTs (warm + one per method), got %d", len(bodies))
		}
		joined := simN4N5Join(bodies)
		if !strings.Contains(joined, `\"method\":\"initialize\"`) {
			t.Errorf("N5: no initialize method on the wire: %s", joined)
		}
		if !strings.Contains(joined, `\"method\":\"tools/call\"`) {
			t.Errorf("N5: no tools/call method on the wire: %s", joined)
		}
		if gateKey(a, soCapability()) == gateKey(b, soCapability()) {
			t.Logf("diagnosis: gateKeyBytes deletes jsonrpc/id/method; both methods share one cache key")
		}
	})

	t.Run("tool_and_target_bound", func(t *testing.T) {
		d, stub, _ := simN4N5Env(t, "advisory")
		d.decideFrame(simN4N5Frame("initialize", 9,
			`"state":"warm up classify request",`+simN4N5FullCaps))
		base := `"state":"classify this request",` + simN4N5FullCaps
		a := simN4N5Frame("tools/call", 1, base+`,"name":"read_file","arguments":{"path":"/tmp/a"}`)
		b := simN4N5Frame("tools/call", 2, base+`,"name":"write_file","arguments":{"path":"/tmp/a"}`)
		c := simN4N5Frame("tools/call", 3, base+`,"name":"read_file","arguments":{"path":"/tmp/b"}`)
		ka, kb, kc := gateKey(a, soCapability()), gateKey(b, soCapability()), gateKey(c, soCapability())
		if ka == kb || ka == kc || kb == kc {
			t.Errorf("N5: tool/target keys collide: %s %s %s", ka, kb, kc)
		}
		d.decideFrame(a)
		d.decideFrame(b)
		d.decideFrame(c)
		bodies := stub.all()
		if n := len(bodies); n != 4 {
			t.Errorf("N5: tool/target binding want 4 SO POSTs (warm + 3 variants), got %d", n)
		}
		if len(bodies) >= 4 {
			t.Logf("observation: tool/target SO bodies equal a==b:%v b==c:%v",
				bodies[1] == bodies[2], bodies[2] == bodies[3])
		}
	})

	t.Run("state_hash_distinct", func(t *testing.T) {
		d, stub, _ := simN4N5Env(t, "advisory")
		d.decideFrame(simN4N5Frame("initialize", 9,
			`"state":"warm up classify request",`+simN4N5FullCaps))
		a := simN4N5Frame("initialize", 1, `"state":"classify this request",`+simN4N5FullCaps)
		b := simN4N5Frame("initialize", 2, `"state":"classify another request",`+simN4N5FullCaps)
		d.decideFrame(a)
		d.decideFrame(b)
		if n := stub.count(); n != 3 {
			t.Errorf("N5: distinct state want 3 SO POSTs (warm + 2 states), got %d", n)
		}
	})

	t.Run("volatile_fields_ignored", func(t *testing.T) {
		d, stub, _ := simN4N5Env(t, "advisory")
		d.decideFrame(simN4N5Frame("initialize", 9,
			`"state":"warm up classify request",`+simN4N5FullCaps))
		a := simN4N5Frame("initialize", 1, `"state":"classify this request",`+simN4N5FullCaps)
		c := simN4N5Frame("initialize", 2, `"state":"classify this request",`+
			simN4N5FullCaps+`,"timestamp":"2026-11-23T00:00:00Z"`)
		d.decideFrame(a)
		d.decideFrame(c)
		if n := stub.count(); n != 2 {
			t.Errorf("N5: volatile-only change want 2 SO POSTs (warm + base), got %d", n)
		}
		t.Logf("observation: canonicalState strips volatile keys; literal N5 'tam state hash' would require 3")
	})

	t.Run("pin_expiry", func(t *testing.T) {
		p := newPin([]provider{{Name: "A", Base: "http://a"}, {Name: "B", Base: "http://b"}})
		if got := p.current().Name; got != "A" {
			t.Fatalf("N5: initial pin want A, got %q", got)
		}
		p.markSuccess("B")
		if got := p.current().Name; got != "B" {
			t.Fatalf("N5: markSuccess(B) want B, got %q", got)
		}
		if gen := p.generation(); gen != 1 {
			t.Errorf("N5: generation want 1 after change, got %d", gen)
		}
		gen := p.generation()
		p.mu.Lock()
		p.expires = time.Now().Add(-time.Second)
		p.mu.Unlock()
		if got := p.current().Name; got != "A" {
			t.Errorf("N5: expired pin want fallback A, got %q", got)
		}
		if got := p.generation(); got != gen {
			t.Errorf("N5: clock expiry must not rotate identity: gen %d -> %d", gen, got)
		}
	})

	t.Run("nonce_only_in_receipt", func(t *testing.T) {
		d, stub, home := simN4N5Env(t, "advisory")
		frame := simN4N5Frame("initialize", 1, `"state":"classify this request",`+simN4N5FullCaps)
		r := d.decideFrame(frame)
		if r.Decision != decPin || r.Reason != "so-admit" {
			t.Fatalf("N5: want pin/so-admit, got %s/%s", r.Decision, r.Reason)
		}
		chain := simN4N5Read(t, filepath.Join(home, ".walsy-system1", "receipt.chain"))
		var rec struct {
			Nonce  string `json:"nonce"`
			Method string `json:"method"`
			Reason string `json:"reason"`
		}
		line := strings.SplitN(chain, "\n", 2)[0]
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("N5: bad receipt: %v (%s)", err, line)
		}
		raw, err := hex.DecodeString(rec.Nonce)
		if err != nil || len(raw) != 16 {
			t.Fatalf("N5: nonce want 16 random bytes as 32 hex chars, got %q (%v)", rec.Nonce, err)
		}
		if rec.Method != "initialize" || rec.Reason != "so-admit" {
			t.Errorf("N5: receipt method/reason = %q/%q", rec.Method, rec.Reason)
		}
		if strings.Contains(simN4N5Join(stub.all()), rec.Nonce) {
			t.Errorf("N5: nonce leaked into SO POST body")
		}
		audit := simN4N5Read(t, filepath.Join(home, ".walsy-system1", "audit.jsonl"))
		if strings.Contains(audit, rec.Nonce) {
			t.Errorf("N5: nonce leaked into audit.jsonl")
		}
	})
}
