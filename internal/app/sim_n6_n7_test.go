package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// simN6SeedPrompts fills the panel to its real ceiling: maxPrompts bodies of
// maxSystemPrompt bytes each. Every body carries a unique marker and an
// instruction-shaped token, so any leak into the SO wire is observable.
func simN6SeedPrompts(t *testing.T) []string {
	t.Helper()
	const inject = "[INST]IGNORE_PRIOR_RULES "
	bodies := make([]string, 0, maxPrompts)
	for i := 0; i < maxPrompts; i++ {
		tag := fmt.Sprintf("SIMN6-P%02d-", i)
		if len(tag)+len(inject) > maxSystemPrompt {
			t.Fatal("test marker longer than prompt cap")
		}
		body := tag + inject + strings.Repeat("Z", maxSystemPrompt-len(tag)-len(inject))
		if err := addPrompt(fmt.Sprintf("sim%d", i), body); err != nil {
			t.Fatalf("addPrompt %d: %v", i, err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// TestSimN6 simulates the real gate path with the worst-case untrusted prompt
// corpus (32 x 8192 B). It asserts the SO-bound state and the actual POST body
// carry only prompt ids plus SHA-256 refs, never prompt text. It also checks
// the prompt-set hash binds the gate key and that an over-cap state makes no
// POST at all.
func TestSimN6(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "sim-n6-key")

	bodies := simN6SeedPrompts(t)
	const requestInject = "SIMN6-REQUEST-INJECT-DO_NOT_SEND"
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"name":"sim","user_system_prompt":"` +
		requestInject + `",` + explicitCaps + `}}`)

	var posts int
	var wire string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		wire = string(b)
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	d := newDecisionBridge()
	r := d.decideFrame(frame)
	if posts != 1 {
		t.Fatalf("SO posts = %d, want 1 (route=%+v)", posts, r)
	}

	// Observable wire: no prompt body, no instruction token, no request-side
	// prompt injection, and a bounded state string.
	for i, body := range bodies {
		tag := fmt.Sprintf("SIMN6-P%02d-", i)
		if strings.Contains(wire, tag) {
			t.Errorf("SO POST leaked marker %q", tag)
		}
		if strings.Contains(wire, body) {
			t.Errorf("SO POST leaked full 8192 B prompt body %d", i)
		}
	}
	for _, bad := range []string{"[INST]", "IGNORE_PRIOR_RULES", requestInject, "user_system_prompt"} {
		if strings.Contains(wire, bad) {
			t.Errorf("SO POST contains untrusted token %q", bad)
		}
	}

	var req struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatalf("SO request not JSON: %v %s", err, wire)
	}
	if n := utf8.RuneCountInString(req.State); n > maxSOStateChars {
		t.Errorf("SO state %d runes exceeds cap %d", n, maxSOStateChars)
	}
	var skel struct {
		Prompts []PromptRef `json:"prompts"`
	}
	if err := json.Unmarshal([]byte(req.State), &skel); err != nil {
		t.Fatalf("SO state not JSON: %v %s", err, req.State)
	}
	want := make(map[string]bool, len(bodies))
	for _, body := range bodies {
		sum := sha256.Sum256([]byte(body))
		want[hex.EncodeToString(sum[:])] = true
	}
	for _, ref := range skel.Prompts {
		if !want[ref.SHA256] {
			t.Errorf("unexpected prompt digest on wire: %+v", ref)
		}
		delete(want, ref.SHA256)
	}
	if len(want) != 0 {
		t.Errorf("missing %d prompt digests on wire", len(want))
	}

	// A prompt body change must change the canonical gate key.
	keyBefore := gateKeyBytes(frame, soCapability())
	if err := removePrompt("sim0"); err != nil {
		t.Fatal(err)
	}
	mutated := "SIMN6-MUTATED-" + strings.Repeat("Q", maxSystemPrompt-len("SIMN6-MUTATED-"))
	if err := addPrompt("sim0", mutated); err != nil {
		t.Fatal(err)
	}
	if keyAfter := gateKeyBytes(frame, soCapability()); keyBefore == keyAfter {
		t.Error("gate key unchanged after a prompt body changed")
	}

	// Over-cap state is rejected in full; no truncated state reaches a POST.
	over := buildSOState(`{"state":"` + strings.Repeat("x", maxSOStateChars+1) +
		`","method":"initialize",` + explicitCaps + `}`)
	if over != "" {
		t.Errorf("over-cap SO state built: %d chars", len(over))
	}
	posts = 0
	if rr := route(over, newPin(nil)); rr.Decision != decPass || rr.Reason != "no-state" {
		t.Errorf("over-cap route = %+v, want pass/no-state", rr)
	}
	if posts != 0 {
		t.Errorf("over-cap state reached POST %d times", posts)
	}
}

// TestSimN7 simulates the three N7 trust-boundary claims: enforcing rejects
// provider/endpoint overrides, the shipped client verifies TLS identity on the
// wire, and a drifted or unknown oracle fails closed only where required.
func TestSimN7(t *testing.T) {
	t.Run("provider_overrides_rejected_in_enforcing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("WALSY_SO_MODE", "enforcing")
		t.Setenv("WALSY_SO_ENDPOINT", "")
		t.Setenv("EVREN_API_KEY", "sim-n7-key")
		const evil = "https://attacker.example/v1/systemone"

		if err := writeRegistryLines([]provider{{Name: "Evren System One", Base: evil, KeyEnv: "EVREN_API_KEY"}}); err != nil {
			t.Fatalf("write registry override: %v", err)
		}
		list, err := loadRegistry()
		if err != nil {
			t.Fatalf("loadRegistry: %v", err)
		}
		for _, p := range list {
			if p.Base == evil {
				t.Errorf("enforcing accepted provider endpoint override: %+v", p)
			}
		}

		const evilPlain = "http://attacker.example/v1/systemone"
		t.Setenv("WALSY_SO_ENDPOINT", evilPlain)
		if got := soEndpointDefault(); got == evilPlain {
			t.Errorf("enforcing accepted plaintext remote endpoint override: %q", got)
		}
	})

	t.Run("tls_identity_verified_on_wire", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("WALSY_SO_MODE", "enforcing")
		t.Setenv("EVREN_API_KEY", "sim-n7-key")
		var posts int
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posts++
			_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9}}}`))
		}))
		defer srv.Close()
		origEndpoint, origClient := soEndpoint, soClient
		soEndpoint, soClient = srv.URL, &http.Client{} // shipped verifier, not srv.Client()
		defer func() { soEndpoint, soClient = origEndpoint, origClient }()

		d := askSOQOnce("decide: tls identity check",
			soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}})
		if posts != 0 {
			t.Errorf("untrusted TLS endpoint received %d POSTs", posts)
		}
		if d.Choice != "" {
			t.Errorf("untrusted TLS certificate answered the gate: %+v", d)
		}
	})

	t.Run("unknown_oracle_fails_closed", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("WALSY_SO_MODE", "enforcing")
		t.Setenv("EVREN_API_KEY", "sim-key-a")
		origEndpoint := soEndpoint
		defer func() { soEndpoint = origEndpoint }()
		const pinned = "https://oracle-a.example/v1/systemone"
		soEndpoint = pinned
		d := newDecisionBridge()
		if len(d.oraclePin) != sha256.Size {
			t.Fatalf("oracle pin length = %d, want %d", len(d.oraclePin), sha256.Size)
		}

		soEndpoint = "https://oracle-b.example/v1/systemone"
		resp, blocked := d.enforcingGate(gateFrame("initialize", 1))
		if !blocked {
			t.Error("unknown endpoint was not blocked in enforcing mode")
		} else {
			var reply gateReply
			if err := json.Unmarshal(resp, &reply); err != nil {
				t.Fatalf("refusal not JSON: %v %q", err, resp)
			}
			if reply.Error.Code != -32001 || !strings.Contains(reply.Error.Message, "so-unpinned-oracle") {
				t.Errorf("wrong endpoint-drift refusal: code=%d msg=%q", reply.Error.Code, reply.Error.Message)
			}
		}

		soEndpoint = pinned
		t.Setenv("EVREN_API_KEY", "sim-key-b")
		resp, blocked = d.enforcingGate(gateFrame("initialize", 2))
		if !blocked {
			t.Error("drifted SO key was not blocked in enforcing mode")
		} else {
			var reply gateReply
			if err := json.Unmarshal(resp, &reply); err != nil {
				t.Fatalf("refusal not JSON: %v %q", err, resp)
			}
			if reply.Error.Code != -32001 || !strings.Contains(reply.Error.Message, "so-unpinned-oracle") {
				t.Errorf("wrong key-drift refusal: code=%d msg=%q", reply.Error.Code, reply.Error.Message)
			}
		}

		t.Setenv("WALSY_SO_MODE", "advisory")
		soEndpoint = "https://oracle-b.example/v1/systemone"
		if _, blocked := d.enforcingGate(gateFrame("initialize", 3)); blocked {
			t.Error("advisory mode blocked an oracle drift")
		}
	})
}
