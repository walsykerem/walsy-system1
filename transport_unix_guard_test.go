package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenUnixRefusesRegularFile(t *testing.T) {
	const sentinel = "keep-me"
	path := filepath.Join(t.TempDir(), "sock")
	if err := os.WriteFile(path, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}

	l, err := listenUnix(path)
	if err == nil {
		l.Close()
		t.Fatal("listenUnix succeeded on a regular file")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file removed: %v", err)
	}
	if string(got) != sentinel {
		t.Fatalf("content changed: got %q", got)
	}
}
