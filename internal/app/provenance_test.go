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

// The store file is created before its first line is written, so an empty one
// is an empty chain, not a tampered ledger that disables every receipt.
func TestReceiptChainEmptyStoreStaysUsable(t *testing.T) {
	resetRouterForTest(t)
	path, err := receiptStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c := loadReceiptChain()
	if c == nil {
		t.Fatal("empty store treated as a broken chain")
	}
	if c.seq != 0 {
		t.Fatalf("seq = %d, want 0", c.seq)
	}
	if _, err := c.appendReceipt(receipt{Event: "gate", Decision: "admit"}); err != nil {
		t.Fatalf("append to empty store: %v", err)
	}
	if c.seq != 1 {
		t.Fatalf("seq after append = %d, want 1", c.seq)
	}
}
