package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
