package main

import (
	"os"
	"strings"
	"testing"
)

// The audit checker must exist, be runnable, and keep audit.jsonl inside a
// throwaway HOME. It must never name the real audit directory.
func TestCheckerAuditScript(t *testing.T) {
	st, err := os.Stat("checker_audit.sh")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0111 == 0 {
		t.Fatalf("checker_audit.sh is not executable: %v", st.Mode())
	}
	b, err := os.ReadFile("checker_audit.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"mktemp -d", `export HOME="$tmp"`, "trap 'rm -rf \"$tmp\"' EXIT", "GOCACHE=", "go test -count=1", "AUDIT-OK"} {
		if !strings.Contains(s, want) {
			t.Fatalf("checker_audit.sh missing %q", want)
		}
	}
	realDir := ".walsy" + "-system1"
	for _, bad := range []string{realDir, "$HOME/" + realDir} {
		if strings.Contains(s, bad) {
			t.Fatalf("checker_audit.sh touches real audit state: %q", bad)
		}
	}
}

// The redaction tokens the checker runs against must stay in the test sources.
func TestAuditRedactionTokensPresent(t *testing.T) {
	for path, token := range map[string]string{
		"../internal/app/item36_test.go":    "sk-item2-DO-NOT-LEAK",
		"../internal/app/relay_log_test.go": "sk-secret",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), token) {
			t.Fatalf("%s lost redaction token %q", path, token)
		}
	}
}
