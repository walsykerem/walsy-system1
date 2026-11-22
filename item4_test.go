package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pingFrame = `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"

// MCP startup wipes only the dedicated cache folder, before the stdin
// transport starts, and leaves persistent state under HOME untouched.
func TestCacheDirCleanedOnMCPStart(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(tmp, cacheDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.json")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale cache file survived MCP start: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("cache dir missing after MCP start: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir mode = %o, want 700", got)
	}
	if data, err := os.ReadFile(persistent); err != nil || string(data) != "keep\n" {
		t.Fatalf("persistent state touched: %q, %v", data, err)
	}
}

// A failed wipe is reported on stderr but never prevents a cold start.
func TestCacheDirRemoveFailureStillOpensTransport(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, cacheDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.so"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })

	var stderr bytes.Buffer
	prepareCacheDir(&stderr)
	if !strings.Contains(stderr.String(), "cache remove:") {
		t.Fatalf("remove failure not reported on stderr: %q", stderr.String())
	}
	out := &strings.Builder{}
	runToolsServer(strings.NewReader(pingFrame), out)
	if out.Len() == 0 {
		t.Fatal("stdin transport did not open after cache remove failure")
	}
}
