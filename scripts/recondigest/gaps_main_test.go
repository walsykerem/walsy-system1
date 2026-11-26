package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// main ends in os.Exit, so it can only be exercised in a child process; the
// helper below runs main against a minimal valid report.
func TestGapScriptMain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte(`{"digest":"abc123","kind":"macho"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGapScriptMainHelper$")
	cmd.Env = append(os.Environ(), "GAP_SCRIPT_MAIN_TARGET="+p)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child exit: %v\n%s", err, out)
	}
	for _, want := range []string{"lrecon digest: abc123", "lrecon kind: macho"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("child output = %q, want %q", out, want)
		}
	}
}

func TestGapScriptMainHelper(t *testing.T) {
	target := os.Getenv("GAP_SCRIPT_MAIN_TARGET")
	if target == "" {
		return
	}
	os.Args = []string{"recondigest", target}
	main()
}
