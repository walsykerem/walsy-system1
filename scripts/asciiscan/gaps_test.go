package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only bytes >= 0x80 count; the reported line must be 1-based even after
// newlines. Neither branch had a test.
func TestGapScriptHighByteLineNumbers(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		code    int
		wantOut string
		line    string
	}{
		{"first line", []byte("\xffboot"), 1, "", ":1"},
		{"third line", []byte("a\nb\n\x80c\n"), 1, "", ":3"},
		{"tab and del are ascii", []byte("a\tb\x7f c\n"), 0, "ascii clean: 1 files\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "scan.txt")
			if err := os.WriteFile(p, tc.content, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := run([]string{p}, &out, &errOut); code != tc.code {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, tc.code, errOut.String())
			}
			if out.String() != tc.wantOut {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.wantOut)
			}
			if tc.line == "" {
				if errOut.Len() != 0 {
					t.Fatalf("stderr = %q, want empty", errOut.String())
				}
				return
			}
			if !strings.Contains(errOut.String(), "non-ascii: "+p+tc.line+"\n") {
				t.Fatalf("stderr = %q, want non-ascii at %s", errOut.String(), tc.line)
			}
		})
	}
}

// One bad file must not stop the scan of later files; an empty list is still
// a clean run with a zero count.
func TestGapScriptScansAllFilesAndEmptyList(t *testing.T) {
	dir := t.TempDir()
	bad1 := filepath.Join(dir, "a.txt")
	bad2 := filepath.Join(dir, "b.txt")
	clean := filepath.Join(dir, "c.txt")
	if err := os.WriteFile(bad1, []byte("\x80"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad2, []byte("ok\n\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clean, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{bad1, clean, bad2}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", out.String())
	}
	for _, want := range []string{"non-ascii: " + bad1 + ":1\n", "non-ascii: " + bad2 + ":2\n"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr = %q, want %q", errOut.String(), want)
		}
	}
	if strings.Contains(errOut.String(), "ascii clean") {
		t.Fatalf("clean summary printed on failure: %q", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(nil, &out, &errOut); code != 0 {
		t.Fatalf("empty list exit = %d, want 0", code)
	}
	if want := "ascii clean: 0 files\n"; out.String() != want {
		t.Fatalf("empty list stdout = %q, want %q", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("empty list stderr = %q", errOut.String())
	}
}
