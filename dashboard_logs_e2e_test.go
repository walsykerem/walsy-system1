package main

// Dashboard log E2E. Drives the real handler over loopback httptest with an
// isolated HOME. RUN COMBO and a tools-bridge decision must land on the same
// log surface; no live endpoint, key or browser is involved.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestE2ERunComboAndMCPDecisionReachDashboardLogs(t *testing.T) {
	srv, home := feServer(t)
	c := feClient(t)

	var hits atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()

	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf("LocalProbe,%s,E2E_DUMMY_KEY\n", probe.URL)
	if err := os.WriteFile(filepath.Join(dir, "providers.txt"), []byte(reg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte("LocalProbe\tlocal-model\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// The sheet's RUN button posts here; this is the exact control path.
	run := feForm(t, c, srv, "/ui/combo/run", nil)
	if run.code != http.StatusOK {
		t.Fatalf("POST /ui/combo/run = %d: %s", run.code, run.body)
	}
	if !strings.Contains(run.body, "WINNER ON ROUND") || hits.Load() == 0 {
		t.Fatalf("combo did not probe the local endpoint: %s", run.body)
	}

	// The tools bridge writes this same audit file for an MCP decision.
	routerAudit("gate", "tool", "system1_decide", "decision", "admit",
		"reason", "so-go", "source", "mcp")

	api := feGet(t, c, srv, "/api/logs")
	if api.code != http.StatusOK {
		t.Fatalf("GET /api/logs = %d", api.code)
	}
	for _, want := range []string{
		`"method":"combo"`, `"event":"gate"`, `"tool":"system1_decide"`,
		`"decision":"admit"`, `"reason":"so-go"`, `"source":"mcp"`,
	} {
		if !strings.Contains(api.body, want) {
			t.Errorf("/api/logs missing %s: %s", want, api.body)
		}
	}
	if strings.Contains(strings.ToLower(api.body), "E2E_DUMMY_KEY") {
		t.Error("/api/logs leaked the probe key")
	}

	// Both transports share one LOGS sheet, even while the in-memory ring
	// holds the combo call.
	ui := feGet(t, c, srv, "/ui/logs")
	if ui.code != http.StatusOK {
		t.Fatalf("GET /ui/logs = %d", ui.code)
	}
	for _, want := range []string{"combo", "system1_decide", "admit", "so-go"} {
		if !strings.Contains(ui.body, want) {
			t.Errorf("/ui/logs missing %q", want)
		}
	}
}

func TestE2EBlockedControlsStayBlocked(t *testing.T) {
	srv, _ := feServer(t)

	// Cross-origin key probing must be rejected like every other write.
	bad := feCrossOrigin(t, feClient(t), srv, "/api/keytest",
		`{"provider":"TEST-P","model":"m1"}`)
	if bad.code != http.StatusBadRequest {
		t.Errorf("cross-origin POST /api/keytest = %d, want 400", bad.code)
	}
	// Same-origin-shaped CLI request (no Origin) still works.
	ok := feJSON(t, feClient(t), srv, "/api/keytest",
		`{"provider":"TEST-P","model":"m1"}`)
	if ok.code != http.StatusOK {
		t.Errorf("POST /api/keytest = %d, want 200: %s", ok.code, ok.body)
	}
}
