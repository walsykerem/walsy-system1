package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// main ends in os.Exit, so it can only be exercised in a child process; the
// helper below runs main with a controlled os.Args.
func TestGapScriptMain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "clean.txt")
	if err := os.WriteFile(p, []byte("plain ascii only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGapScriptMainHelper$")
	cmd.Env = append(os.Environ(), "GAP_SCRIPT_MAIN_TARGET="+p)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child exit: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ascii clean: 1 files") {
		t.Fatalf("child output = %q, want ascii clean line", out)
	}
}

func TestGapScriptMainHelper(t *testing.T) {
	target := os.Getenv("GAP_SCRIPT_MAIN_TARGET")
	if target == "" {
		return
	}
	os.Args = []string{"asciiscan", target}
	main()
}
