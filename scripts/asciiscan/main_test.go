package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A listed path that cannot be scanned must fail, not be silently skipped.
func TestRunRejectsMissingAndNonRegularPaths(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(regular, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{filepath.Join(dir, "missing.go"), dir} {
		var out, errOut bytes.Buffer
		if code := run([]string{regular, bad}, &out, &errOut); code != 1 {
			t.Fatalf("path %q: exit = %d, want 1", bad, code)
		}
		if !strings.Contains(errOut.String(), bad) {
			t.Fatalf("path %q: stderr = %q, want path named", bad, errOut.String())
		}
		if out.Len() != 0 {
			t.Fatalf("path %q: stdout = %q, want empty on failure", bad, out.String())
		}
	}
}

// The normal case keeps its exact output format and exit status.
func TestRunCleanOutputUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(p, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{p}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, errOut.String())
	}
	if want := "ascii clean: 1 files\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}
