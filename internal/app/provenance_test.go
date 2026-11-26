package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFA15HealthFingerprint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := healthFingerprint()
	if h["ok"] != true {
		t.Fatal("health lost ok")
	}
	if len(h["receipt_sha256"].(string)) != 64 {
		t.Fatal("receipt not sha256")
	}
	if len(h["items_sha256"].(string)) != 64 {
		t.Fatal("items digest not sha256")
	}
	if h["so_mode"] != "advisory" && h["so_mode"] != "enforcing" {
		t.Fatalf("so_mode %v", h["so_mode"])
	}
	if strings.Contains(h["receipt_sha256"].(string), " ") {
		t.Fatal("digest has whitespace")
	}
}

func TestN12UsageCredits(t *testing.T) {
	if c, ok := usageCredits(map[string]any{"credits": 2.0}); !ok || c != 2 {
		t.Fatalf("flat credits = %v %v", c, ok)
	}
	if c, ok := usageCredits(map[string]any{"evren": map[string]any{"credits": 3.5}}); !ok || c != 3.5 {
		t.Fatalf("nested credits = %v %v", c, ok)
	}
	if _, ok := usageCredits(map[string]any{"other": "x"}); ok {
		t.Fatal("missing credits reported present")
	}
}

func TestN12CallAuditCarriesLatencyAndCredits(t *testing.T) {
	resetRouterForTest(t)
	resetLogs()
	defer resetLogs()
	logCall("systemone", "ok", 12*time.Millisecond, 100, 200,
		map[string]any{"evren": map[string]any{"credits": 2.0}})
	b, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"latency_ms":"12"`, `"credits":"2"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("audit missing %s: %q", want, b)
		}
	}
	totals, entries := logsView()
	if len(entries) != 1 || entries[0].Credits != 2 {
		t.Fatalf("credits not retained: %+v", entries)
	}
	if totals.CostPerCall != 2 {
		t.Fatalf("cost per call = %v", totals.CostPerCall)
	}
}
