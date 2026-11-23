package main

// Genuine simulation for MANIFEST items N12 and N15.
// No production file is touched. Every test drives the real SO gate, the real
// audit writer and the real dashboard renderer through httptest handlers.
// Assertions express the REQUIRED behavior, so a missing feature fails here.

import (
	"encoding/json"
	"fmt"
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

const simN12N15OKBody = `{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.95}},"usage":{"evren":{"credits":2.5}}}`

// simN12N15Env makes HOME hermetic, clears config mode, resets router/log state.
func simN12N15Env(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("EVREN_API_KEY", "sim-test-key") // fake value; no real key read
	routerReset()
	t.Cleanup(routerReset)
	resetLogs()
	t.Cleanup(resetLogs)
	return home
}

// simN12N15SO points the real gate at an httptest SO stub and restores it after.
func simN12N15SO(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	old := soEndpoint
	soEndpoint = srv.URL
	t.Cleanup(func() { soEndpoint = old; srv.Close() })
	return srv
}

func simN12N15Question() soQuestion {
	return soQuestion{Type: "choice", Criteria: map[string]string{
		"yes": "admit", "no": "pass", "other": "ambiguous",
	}}
}

// N12: a real SO call must append latency_ms + credits to audit.jsonl, and the
// dashboard must render the resulting cost-per-decision gauge.
func TestSimN12CostMeter(t *testing.T) {
	home := simN12N15Env(t)
	simN12N15SO(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, simN12N15OKBody)
	})

	d := askSOQOnce("classify this", simN12N15Question())
	if d.Choice != "no" {
		t.Fatalf("stub answer not observed through real gate: %+v", d)
	}

	raw, err := os.ReadFile(filepath.Join(home, routerDir, routerAuditName))
	if err != nil {
		t.Fatalf("audit.jsonl not written by real SO call: %v", err)
	}
	var call map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) == nil && row["event"] == "call" && row["method"] == "systemone" {
			call = row
		}
	}
	if call == nil {
		t.Fatalf("no systemone call row in audit: %s", raw)
	}
	if _, err := strconv.ParseInt(call["latency_ms"], 10, 64); err != nil {
		t.Fatalf("latency_ms missing or non-numeric: %q in %s", call["latency_ms"], raw)
	}
	if call["credits"] != "2.5" {
		t.Fatalf("credits = %q, want 2.5: %s", call["credits"], raw)
	}

	totals, _ := logsView()
	if totals.CostPerCall != 2.5 {
		t.Fatalf("cost per decision = %v, want 2.5", totals.CostPerCall)
	}
	rec := httptest.NewRecorder()
	uiLogsSheet(rec)
	if !strings.Contains(rec.Body.String(), "2.5 CREDITS/CALL") {
		t.Fatalf("dashboard missing cost-per-decision gauge; body=%s", rec.Body.String())
	}
}

// N15a: the SO deadline must be ~150ms, so a slow endpoint is cut off before it
// can answer. Production soTimeout is still 10s; this test fails on that gap.
func TestSimN15Deadline(t *testing.T) {
	simN12N15Env(t)
	simN12N15SO(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(800 * time.Millisecond)
		_, _ = fmt.Fprint(w, simN12N15OKBody)
	})
	d := askSOQOnce("classify this", simN12N15Question())
	if soTimeout > 400*time.Millisecond || !d.TimedOut {
		t.Fatalf("N15 deadline NOT ~150ms: soTimeout=%v TimedOut=%v; want cutoff before the 800ms reply", soTimeout, d.TimedOut)
	}
}

// N15b: sequential SO calls must reuse one TCP connection (keep-alive client).
func TestSimN15KeepAlive(t *testing.T) {
	simN12N15Env(t)
	var mu sync.Mutex
	seen := map[string]int{}
	simN12N15SO(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.RemoteAddr]++
		mu.Unlock()
		_, _ = fmt.Fprint(w, simN12N15OKBody)
	})
	for i := 0; i < 3; i++ {
		d := askSOQOnce("classify this", simN12N15Question())
		if d.Choice != "no" {
			t.Fatalf("call %d not observed: %+v", i, d)
		}
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("N15 keep-alive: %d distinct TCP connections for 3 sequential calls, want 1", n)
	}
}

// N15c: cheapest endpoint first requires cost metadata on a provider slot.
// Real httptest endpoints are declared with an explicit per-call cost column;
// the registry must accept it and the transport must probe the cheap one first.
func TestSimN15CheapestFirst(t *testing.T) {
	home := simN12N15Env(t)
	cheap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer cheap.Close()
	pricey := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer pricey.Close()

	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "providers.txt")
	data := fmt.Sprintf("Cheap,%s,TEST_KEY,1\nPricey,%s,TEST_KEY,100\n", cheap.URL, pricey.URL)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := loadRegistryFrom(path)
	if err != nil {
		t.Fatalf("N15 cheapest-first: registry cannot express endpoint cost: %v", err)
	}
	steps := make([]ComboStep, 0, len(list))
	for _, p := range list {
		if strings.HasPrefix(p.Base, "http://127.0.0.1") || strings.HasPrefix(p.Base, "http://localhost") {
			steps = append(steps, ComboStep{Name: p.Name, Base: p.Base})
		}
	}
	if len(steps) != 2 {
		t.Fatalf("two local endpoints expected, got %d", len(steps))
	}
	got := RunCombo(steps, 0)
	if len(got.Steps) == 0 || got.Steps[0].Name != "Cheap" {
		t.Fatalf("N15 cheapest-first: first probed = %+v, want Cheap", got.Steps)
	}
}
