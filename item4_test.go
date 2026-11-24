package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pingFrame = `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

// Decision caching is process-local. MCP start touches no TMPDIR cache folder;
// persistent state under HOME stays untouched.
func TestNoDiskCacheDirOnMCPStart(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	home := t.TempDir()
	t.Setenv("HOME", home)

	persistentDir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(persistentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	persistent := filepath.Join(persistentDir, "providers.txt")
	if err := os.WriteFile(persistent, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	runToolsServer(strings.NewReader(pingFrame), out)
	if out.Len() == 0 {
		t.Fatal("stdin transport never opened")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("MCP start created temp files: %v", entries)
	}
	if data, err := os.ReadFile(persistent); err != nil || string(data) != "keep\n" {
		t.Fatalf("persistent state touched: %q, %v", data, err)
	}
}

// A cold start never needs a disk cache directory; the transport still opens.
func TestColdStartNeedsNoCacheDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	out := &strings.Builder{}
	runToolsServer(strings.NewReader(pingFrame), out)
	if out.Len() == 0 {
		t.Fatal("stdin transport did not open on cold start")
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cold start created temp files: %v", entries)
	}
}
