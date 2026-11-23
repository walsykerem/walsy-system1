package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Empty or malformed reports must exit non-zero and never print an empty
// "lrecon digest:" line that verify.sh cannot catch.
func TestRunFailsClosedOnBadDigest(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"empty digest", `{"digest":"","kind":"x"}`},
		{"malformed json", `{not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := run([]string{p}, &out, &errOut); code == 0 {
				t.Fatal("exit = 0, want non-zero")
			}
			if strings.Contains(out.String(), "lrecon digest:") {
				t.Fatalf("stdout = %q, want no digest line on failure", out.String())
			}
			if errOut.Len() == 0 {
				t.Fatal("empty stderr, want clear error")
			}
		})
	}
}

func TestRunMissingFileFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{filepath.Join(t.TempDir(), "no.json")}, &out, &errOut); code == 0 {
		t.Fatal("exit = 0, want non-zero")
	}
}
