package app

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A populated chain truncated in place must fail closed, not restart at seq 1.
func TestLoadReceiptChainRejectsTruncatedPopulatedStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	c := loadReceiptChain()
	if c == nil {
		t.Fatal("fresh chain should load")
	}
	if _, err := c.appendReceipt(receipt{Event: "test"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	path, err := receiptStorePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if loadReceiptChain() != nil {
		t.Fatal("truncated populated store must fail closed")
	}
}

// A store that never received a completed append is still a fresh chain.
func TestLoadReceiptChainAcceptsInitialEmptyStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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
	if loadReceiptChain() == nil {
		t.Fatal("initial empty store should load as a fresh chain")
	}
}

// A tail dropped or rewritten at a line boundary still parses as a shorter,
// internally consistent chain: only the head sidecar records the newest line.
func TestLoadReceiptChainRejectsTailRollback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chain := &receiptChain{}
	for _, d := range []string{decPin, decPin} {
		if _, err := chain.appendReceipt(receipt{Event: "decision", Decision: d, Method: "tools/call"}); err != nil {
			t.Fatal(err)
		}
	}
	path, err := receiptStorePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	write := func(lines []string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := loadReceiptChain(); got != nil {
			t.Fatalf("rolled-back chain loaded as seq %d", got.seq)
		}
	}
	write(lines[:1]) // newest receipt deleted
	var last receipt
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil {
		t.Fatal(err)
	}
	last.Decision = decPass
	body, err := canonicalJSON(last)
	if err != nil {
		t.Fatal(err)
	}
	write([]string{lines[0], string(body)}) // newest receipt rewritten
}

// A crash between the chain line write and the head sidecar update leaves the
// sidecar one line behind. The chain must keep loading and accept new receipts.
func TestLoadReceiptChainAcceptsStaleSidecarAfterInterruptedAppend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := loadReceiptChain()
	if c == nil {
		t.Fatal("fresh chain should load")
	}
	for i := 0; i < 2; i++ {
		if _, err := c.appendReceipt(receipt{Event: "test"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	path, err := receiptStorePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var first receipt
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	body, err := canonicalJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	sum := chainHash(nil, body)
	hp, err := receiptHeadPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, []byte(hex.EncodeToString(sum[:])+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloaded := loadReceiptChain()
	if reloaded == nil {
		t.Fatal("stale sidecar one line behind must load after interrupted append")
	}
	if reloaded.seq != 2 {
		t.Fatalf("seq = %d, want 2", reloaded.seq)
	}
}

// A post-write failure (a foreign evidence file is refused) must leave the
// in-memory chain on the durable line, or the next append reuses its seq and
// the store stops loading.
func TestAppendReceiptAdvancesAfterPostWriteFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	evidence, err := receiptPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(evidence), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence, []byte("foreign\n"), 0600); err != nil {
		t.Fatal(err)
	}

	c := loadReceiptChain()
	if c == nil {
		t.Fatal("fresh chain should load")
	}
	if _, err := c.appendReceipt(receipt{Event: "test"}); err == nil {
		t.Fatal("foreign evidence file must fail the append")
	}
	if c.seq != 1 || len(c.head) == 0 {
		t.Fatalf("chain state = seq %d head %x, want the durable line counted", c.seq, c.head)
	}
	if _, err := c.appendReceipt(receipt{Event: "test"}); err == nil {
		t.Fatal("second append must fail on the same foreign file")
	}
	if loadReceiptChain() == nil {
		t.Fatal("store must stay loadable after a post-write failure")
	}
}
