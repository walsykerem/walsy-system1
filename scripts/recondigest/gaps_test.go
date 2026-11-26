package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The digest fallback chain and the exact verify.sh lines were untested.
func TestGapScriptDigestFallbacksAndOutput(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"digest wins", `{"digest":"sha-main","target":{"file_sha256":"sha-file"},"uuid":"u","kind":"macho"}`,
			"lrecon digest: sha-main\nlrecon kind: macho\n"},
		{"file sha fallback", `{"digest":"","target":{"file_sha256":"sha-file"},"uuid":"u","kind":"elf"}`,
			"lrecon digest: sha-file\nlrecon kind: elf\n"},
		{"uuid fallback", `{"digest":"","target":{"file_sha256":""},"uuid":"u","kind":"wasm"}`,
			"lrecon digest: u\nlrecon kind: wasm\n"},
		{"empty kind still printed", `{"uuid":"u"}`,
			"lrecon digest: u\nlrecon kind: \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := run([]string{p}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
			}
			if out.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.want)
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", errOut.String())
			}
		})
	}
}

// A wrong argument count must be a usage error, not a read of a random path.
func TestGapScriptUsageRequiresOneArg(t *testing.T) {
	for _, args := range [][]string{nil, {"a.json", "b.json"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("args %v: exit = %d, want 2", args, code)
		}
		if !bytes.Contains(errOut.Bytes(), []byte("usage: recondigest <report.json>")) {
			t.Fatalf("args %v: stderr = %q", args, errOut.String())
		}
		if out.Len() != 0 {
			t.Fatalf("args %v: stdout = %q", args, out.String())
		}
	}
}
