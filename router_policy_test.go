package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetRouterForTest(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	routerReset()
	t.Cleanup(routerReset)
}

func clearRouterEnv(t *testing.T, key string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func writeRouterConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRouterAllowDeny(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_DENY", "read_file,shell")
	routerReset()
	if routerAllow("read_file") || routerAllow("shell") {
		t.Fatal("explicitly denied tool allowed")
	}
	if !routerAllow("fetch") || !routerAllow("") || !routerAllow("bad tool") {
		t.Fatal("default must allow all")
	}

	clearRouterEnv(t, "WALSY_ROUTER_DENY")
	home := os.Getenv("HOME")
	writeRouterConfig(t, home, "so_mode=advisory\nrouter_deny=write_file\n")
	routerReset()
	if routerAllow("write_file") || !routerAllow("read_file") {
		t.Fatal("config deny list ignored")
	}
}

func TestRouterBudgetExhaustion(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "")
	routerReset()
	for i := 0; i < routerDefaultBudget; i++ {
		if !routerBudgetOK() {
			t.Fatalf("budget exhausted early at %d", i)
		}
	}
	if routerBudgetOK() {
		t.Fatal("budget must be bounded")
	}

	t.Setenv("WALSY_ROUTER_RETRY_BUDGET", "2")
	routerReset()
	if !routerBudgetOK() || !routerBudgetOK() || routerBudgetOK() {
		t.Fatal("configured budget not honored")
	}
}

func TestRouterBreakerOpenHalfOpen(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "40ms")
	routerReset()

	routerRecord("tool", false)
	routerRecord("tool", true)
	if routerBreakerOpen("tool") {
		t.Fatal("success must close breaker")
	}
	routerRecord("tool", false)
	routerRecord("tool", false)
	if !routerBreakerOpen("tool") {
		t.Fatal("breaker must open after repeated failures")
	}
	time.Sleep(60 * time.Millisecond)
	if routerBreakerOpen("tool") {
		t.Fatal("half-open probe must be allowed after cooldown")
	}
	if !routerBreakerOpen("tool") {
		t.Fatal("only one half-open probe allowed")
	}
	routerRecord("tool", true)
	if routerBreakerOpen("tool") {
		t.Fatal("success must close half-open breaker")
	}
}

func TestRouterMirrorURL(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_MIRROR_URL", "http://127.0.0.1:9/canary")
	routerReset()
	if got := routerMirrorURL(); got != "http://127.0.0.1:9/canary" {
		t.Fatalf("mirror = %q", got)
	}
	t.Setenv("WALSY_ROUTER_MIRROR_URL", "not-a-url")
	routerReset()
	if got := routerMirrorURL(); got != "" {
		t.Fatalf("invalid mirror accepted: %q", got)
	}
	t.Setenv("WALSY_ROUTER_MIRROR_URL", "")
	routerReset()
	if got := routerMirrorURL(); got != "" {
		t.Fatalf("disabled mirror = %q", got)
	}

	clearRouterEnv(t, "WALSY_ROUTER_MIRROR_URL")
	writeRouterConfig(t, os.Getenv("HOME"), "router_mirror_url=http://localhost:9/shadow\n")
	routerReset()
	if got := routerMirrorURL(); got != "http://localhost:9/shadow" {
		t.Fatalf("config mirror = %q", got)
	}
}

func TestRouterAuditRedaction(t *testing.T) {
	resetRouterForTest(t)
	secret := "sk-secret-abc123"
	routerAudit("decision", "tool", "read_file", "token", secret, "status", "allow", "dangling")
	path := filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, secret) || strings.Contains(body, `"token"`) {
		t.Fatalf("audit leaked secret: %q", body)
	}
	for _, want := range []string{`"event":"decision"`, `"tool":"read_file"`, `"status":"allow"`, `"bucket":"odd_kv"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("audit missing %s: %q", want, body)
		}
	}
	var record map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("audit not JSONL: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("audit mode = %v err = %v", st, err)
	}
}

func TestRouterDiscoveredTools(t *testing.T) {
	resetRouterForTest(t)
	t.Setenv("WALSY_ROUTER_TOOLS", "zeta,read_file,bad tool,zeta")
	t.Setenv("WALSY_ROUTER_DENY", "zeta")
	routerReset()
	got := discoveredTools()
	if len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("discovered = %v", got)
	}
}

func TestRouterBudgetSpendTable(t *testing.T) {
	cases := []struct {
		name   string
		budget string
		max    int
	}{
		{"missing uses default", "", routerDefaultBudget},
		{"malformed uses default", "lots", routerDefaultBudget},
		{"explicit zero", "0", 0},
		{"explicit two", "2", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRouterForTest(t)
			t.Setenv("WALSY_ROUTER_RETRY_BUDGET", tc.budget)
			routerReset()
			for i := 0; i < tc.max; i++ {
				if _, max, spent := routerBudgetSpend(); !spent || max != tc.max {
					t.Fatalf("spend %d failed, max=%d want %d", i, max, tc.max)
				}
			}
			if _, max, spent := routerBudgetSpend(); spent || max != tc.max {
				t.Fatalf("budget not bounded at %d", tc.max)
			}
			if tc.max == 0 {
				return // an explicit zero budget refills to zero
			}
			routerBudgetReset()
			if _, _, spent := routerBudgetSpend(); !spent {
				t.Fatal("reset must refill the budget")
			}
		})
	}
}
