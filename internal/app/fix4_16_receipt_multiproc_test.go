package app

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fix416HelperEnv = "WALSY_FIX4_16_HELPER"

// Re-exec helper: two independent processes append to the same on-disk chain.
// TestMain replaces HOME, so the parent passes it explicitly.
func TestFix416ReceiptHelperProcess(t *testing.T) {
	if os.Getenv(fix416HelperEnv) != "1" {
		return
	}
	home := os.Getenv("WALSY_FIX4_16_HOME")
	child := os.Getenv("WALSY_FIX4_16_CHILD")
	if home == "" || child == "" {
		fmt.Fprintln(os.Stderr, "helper: missing home or child")
		os.Exit(2)
	}
	_ = os.Setenv("HOME", home)
	c := loadReceiptChain()
	if c == nil {
		fmt.Fprintln(os.Stderr, "helper: broken chain")
		os.Exit(3)
	}
	for i := 0; i < 200; i++ {
		r := receipt{Event: "decision", Decision: decPin, Method: "tools/call",
			Reason: "child-" + child, Tool: fmt.Sprintf("child-%s-%d", child, i)}
		if _, err := c.appendReceipt(r); err != nil {
			fmt.Fprintln(os.Stderr, "helper append:", err)
			os.Exit(4)
		}
	}
	if err := os.WriteFile(os.Getenv("WALSY_FIX4_16_MARKER")+child, []byte("done"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "helper marker:", err)
		os.Exit(5)
	}
	// Stay alive until the parent kills one child; stdin stays open.
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

// Two processes x 200 appends, one child killed after work: the survivor's
// receipts must chain onto the killed child's, with no seq/prev collision.
func TestFix416MultiProcessReceiptLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := filepath.Join(home, "child-")
	const n = 200

	cmds := make([]*exec.Cmd, 2)
	stdins := make([]io.WriteCloser, 2)
	logs := make([]bytes.Buffer, 2)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFix416ReceiptHelperProcess$")
		cmd.Env = append(os.Environ(), fix416HelperEnv+"=1", "WALSY_FIX4_16_HOME="+home,
			"WALSY_FIX4_16_CHILD="+strconv.Itoa(i), "WALSY_FIX4_16_MARKER="+marker)
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdins[i] = in
		cmd.Stdout = &logs[i]
		cmd.Stderr = &logs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}

	deadline := time.Now().Add(60 * time.Second)
	for i := range cmds {
		for {
			if _, err := os.Stat(marker + strconv.Itoa(i)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("helper %d did not finish: %s", i, logs[i].String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := cmds[1].Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmds[1].Wait()
	if err := stdins[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmds[0].Wait(); err != nil {
		t.Fatalf("surviving helper: %v\n%s", err, logs[0].String())
	}

	c := loadReceiptChain()
	if c == nil {
		t.Fatal("chain failed verification after concurrent appends and child kill")
	}
	if c.seq != 2*n {
		t.Fatalf("seq = %d, want %d", c.seq, 2*n)
	}
	hp, err := receiptHeadPath()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(rec)), hex.EncodeToString(c.head); got != want {
		t.Fatalf("head sidecar = %s, want %s", got, want)
	}
}

// A killed append leaves an unterminated JSON tail. It must be cut back, not
// treated as a rolled-back chain or as a receipt.
func TestFix416TornTailRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	c := loadReceiptChain()
	if c == nil {
		t.Fatal("fresh chain should load")
	}
	if _, err := c.appendReceipt(receipt{Event: "decision", Decision: decPin}); err != nil {
		t.Fatal(err)
	}
	path, err := receiptStorePath()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":`); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := loadReceiptChain()
	if got == nil || got.seq != 1 {
		t.Fatalf("torn tail not repaired: %#v", got)
	}
	if _, err := got.appendReceipt(receipt{Event: "decision", Decision: decPass}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 2 {
		t.Fatalf("store has %d complete lines, want 2", lines)
	}
	if loadReceiptChain() == nil {
		t.Fatal("chain broken after torn-tail recovery")
	}
}
