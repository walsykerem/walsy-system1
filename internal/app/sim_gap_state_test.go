package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSimGapStateAtomicReplace pins the replacement half of state persistence:
// a save installs a fully written file under a new inode instead of mutating
// the live target in place, so a reader can never observe truncated bytes.
func TestSimGapStateAtomicReplace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := mustStateDir(t)
	path := filepath.Join(dir, "models.txt")

	if err := saveModels(map[string][]string{"P": {"m1"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0600 {
		t.Fatalf("models.txt mode = %o, want 600", before.Mode().Perm())
	}

	if err := saveModels(map[string][]string{"P": {"m2", "m3"}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("saveModels reused the models.txt inode; want temp+rename replacement, not in-place truncation")
	}
	if after.Mode().Perm() != 0600 {
		t.Fatalf("models.txt mode after replace = %o, want 600", after.Mode().Perm())
	}

	raw, err := readStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "P\tm2\nP\tm3\n" {
		t.Fatalf("models.txt = %q, want the complete new content", raw)
	}
	assertNoStateTemp(t, dir)
}

// TestSimGapStateReceiptBindsBinaryDigest makes the build digest non-empty
// (go test leaves it empty) and proves /api/health and the receipt chain both
// bind that exact binary digest. A dropped or hardcoded build_sha fails here.
func TestSimGapStateReceiptBindsBinaryDigest(t *testing.T) {
	home := faW32Env(t)
	faW32SO(t, faW32SOYes)
	if err := savePrompts([]Prompt{{Name: "gap-bind", Text: "gap bind body"}}); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("gap shipped binary"))
	pinned := hex.EncodeToString(sum[:])
	orig := healthBuildSHA
	healthBuildSHA = pinned
	t.Cleanup(func() { healthBuildSHA = orig })

	receiptDigest := func(build string) string {
		h := sha256.New()
		_, _ = h.Write(decisionItemsJSON)
		for _, p := range loadPrompts() {
			_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(p.Name), p.Name, len(p.Text), p.Text)
		}
		_, _ = h.Write([]byte(build))
		return hex.EncodeToString(h.Sum(nil))
	}

	fp := healthFingerprint()
	if got := fp["build_sha"]; got != pinned {
		t.Fatalf("health build_sha = %v, want %s", got, pinned)
	}
	want := receiptDigest(pinned)
	if got := fp["receipt_sha256"]; got != want {
		t.Fatalf("health receipt_sha256 = %v, want %s", got, want)
	}
	if receiptDigest("") == want {
		t.Fatal("empty and pinned build digests hash identically; the binding is not observable")
	}

	d := newDecisionBridge()
	r := d.decideFrame(faW32Frame("gap binary digest binding"))
	if !receiptWorthy(r) {
		t.Fatalf("frame produced no receipt-worthy result: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(home, routerDir, "receipt.chain"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatalf("receipt JSON: %v", err)
	}
	if got := row["build_sha"]; got != pinned {
		t.Fatalf("receipt build_sha = %v, want %s", got, pinned)
	}
	if got := row["receipt_sha256"]; got != want {
		t.Fatalf("receipt receipt_sha256 = %v, want %s", got, want)
	}
	if got, wantItems := row["items_sha256"], fp["items_sha256"]; got != wantItems {
		t.Fatalf("receipt items_sha256 = %v, want %v", got, wantItems)
	}
}
