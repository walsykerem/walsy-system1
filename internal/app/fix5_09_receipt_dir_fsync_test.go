package app

import (
	"os"
	"testing"
)

func TestFix509ReceiptDirFsyncOnCreationOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	real := syncReceiptDir
	calls := 0
	syncReceiptDir = func(dir string) error {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Fatalf("syncReceiptDir(%q): stat = %v, isDir = %v", dir, err, err == nil && st.IsDir())
		}
		calls++
		return nil
	}
	t.Cleanup(func() { syncReceiptDir = real })

	c := loadReceiptChain()
	if c == nil {
		t.Fatal("fresh chain should load")
	}
	if _, err := c.appendReceipt(receipt{Event: "test"}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("first append dir fsync calls = %d, want 3 (store, evidence, head)", calls)
	}
	if _, err := c.appendReceipt(receipt{Event: "test"}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("after second append dir fsync calls = %d, want 3 (no steady-state sync)", calls)
	}
	if got := loadReceiptChain(); got == nil || got.seq != 2 {
		t.Fatalf("chain after two appends = %#v, want seq 2", got)
	}
}
