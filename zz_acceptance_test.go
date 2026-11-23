package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Acceptance checks for the seven contract gaps. All state is local: temp HOME,
// no live System One call, no key material in assertions or output.

func TestAcceptanceGap1ClientWiringSnippet(t *testing.T) {
	s := opencodeMCPSnippet("system1", "stdio", []string{"stdio", "echo"})
	for _, want := range []string{"system1", "-transport", "stdio"} {
		if !strings.Contains(s, want) {
			t.Fatalf("snippet missing %q: %s", want, s)
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatalf("snippet is not valid JSON: %v", err)
	}
}

func TestAcceptanceGap2ModeDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	mode := soMode()
	if mode != "advisory" && mode != "enforcing" {
		t.Fatalf("soMode = %q, want advisory or enforcing", mode)
	}
	if mode != "advisory" {
		t.Fatalf("soMode default = %q, want advisory", mode)
	}
}

func TestAcceptanceGap3TransportFlagsRegistered(t *testing.T) {
	for _, name := range []string{"upstream", "print-client-config"} {
		if flag.Lookup(name) == nil {
			t.Fatalf("flag -%s is not registered", name)
		}
	}
}

func TestAcceptanceGap4CapabilityOptIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")

	c := soCapability()
	if c.NeedLabel || c.NoScratch || c.Safe || c.Reversible ||
		c.Authority || c.Calibrated || c.Phase != "" {
		t.Fatalf("capability default must require explicit opt-in: %+v", c)
	}
}

func TestAcceptanceGap5HTTPOriginAndMethod(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	cases := []struct {
		name string
		mut  func(*http.Request)
	}{
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"method", func(r *http.Request) { r.Header.Set("Mcp-Method", "tools/call") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(frame))
			tc.mut(req)
			mcpHTTPHandler().ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("bad %s accepted with 200", tc.name)
			}
		})
	}
}

func TestAcceptanceGap6RouterAndWeightedCombo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "")

	if !routerAllow("known-tool") {
		t.Fatal("default routerAllow = false, want true")
	}
	if !routerBudgetOK() {
		t.Fatal("default routerBudgetOK = false, want true")
	}
	routerRecord("known-tool", true)
	routerAudit("acceptance", "tool", "known-tool")
	_ = routerMirrorURL()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Weight must survive the persisted combo and drive the probe order, not
	// just exist as a struct field: a single-step assertion cannot see ordering.
	t.Setenv("HOME", t.TempDir())
	if err := saveCombo([]stateStep{
		{Provider: "primary", Model: "low", Weight: 1},
		{Provider: "backup", Model: "high", Weight: 10},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCombo()
	if err != nil || len(loaded) != 2 || loaded[1].Weight != 10 {
		t.Fatalf("weight did not persist: %+v, %v", loaded, err)
	}
	order := comboOrder(toComboSteps(providersResponse{}, loaded))
	if len(order) != 2 || order[0] != 1 {
		t.Fatalf("stored weights do not reorder the combo: order = %v", order)
	}
	out := RunCombo(toComboSteps(providersResponse{Providers: []dashboardProvider{{Name: "primary", Base: srv.URL}, {Name: "backup", Base: srv.URL}}}, loaded), 200*time.Millisecond)
	if out.Winner == "" {
		t.Fatalf("weighted combo produced no winner: %+v", out)
	}
}

func TestAcceptanceGap7PerRequestDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("EVREN_API_KEY", "")
	t.Setenv("WALSY_SO_MODE", "")

	d := newDecisionBridge()
	frames := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`),
	}
	for i, frame := range frames {
		r := d.decide(frame)
		if r.Decision != decPass || !r.Passthrough {
			t.Fatalf("call %d: decision = %+v, want pass-through", i, r)
		}
	}
}
