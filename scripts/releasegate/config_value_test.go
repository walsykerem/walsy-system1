package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValueReadsHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".walsy-system1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	config := "m1_approval_owner=alice\nevren_api_key=dummy-key\n"
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if got := configValue("m1_approval_owner"); got != "alice" {
		t.Fatalf("m1_approval_owner = %q, want %q", got, "alice")
	}
	if got := configValue("evren_api_key"); got != "dummy-key" {
		t.Fatal("evren_api_key not read from home config")
	}
}
