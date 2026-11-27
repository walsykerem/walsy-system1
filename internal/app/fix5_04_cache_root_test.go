package app

import (
	"os"
	"path/filepath"
	"testing"
)

// A state root that becomes unsafe after enforcing is configured must keep
// live checks enforcing, not downgrade to advisory.
func TestFix511UnsafeRootKeepsEnforcing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_APPROVER", "")
	root := filepath.Join(home, ".walsy-system1")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.txt"), []byte("so_mode=enforcing\napprover=ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := soMode(); got != soModeEnforcing {
		t.Fatalf("baseline soMode = %q, want enforcing", got)
	}

	// Loosen root permissions: config must not silently fall back to advisory.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := soMode(); got != soModeEnforcing {
		t.Fatalf("after chmod soMode = %q, want enforcing", got)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}

	// Replace the root with a symlink to a directory holding a permissive config.
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "config.txt"), []byte("so_mode=advisory\napprover=ops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	if got := soMode(); got != soModeEnforcing {
		t.Fatalf("after symlink soMode = %q, want enforcing", got)
	}
}

// Production state has one guarded root. A cache env override or a symlink
// into a generated-artifact cache must never redirect config/model/combo.
func TestFix504StateNeverInCacheRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := t.TempDir()
	t.Setenv("GOCACHE", cache)
	t.Setenv("XDG_CACHE_HOME", cache)

	root := filepath.Join(home, ".walsy-system1")

	// A cache directory masquerading as the state root must be rejected on the
	// config read path, not silently trusted as production config.
	if err := os.WriteFile(filepath.Join(cache, "config.txt"), []byte("so_mode=enforcing\napprover=cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cache, root); err != nil {
		t.Fatal(err)
	}
	if p, err := soConfigPath(); err == nil {
		t.Fatalf("config path %q followed symlink into cache root %q", p, cache)
	}
	if _, err := stateDir(); err == nil {
		t.Fatal("stateDir followed symlink into cache root")
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := soConfigPath()
	if err != nil {
		t.Fatalf("soConfigPath: %v", err)
	}
	if filepath.Dir(cfg) != root {
		t.Fatalf("config path %q outside state root %q", cfg, root)
	}
	if err := saveModels(map[string][]string{"p": {"m"}}); err != nil {
		t.Fatalf("saveModels: %v", err)
	}
	if err := saveCombo([]stateStep{{Provider: "p", Model: "m"}}); err != nil {
		t.Fatalf("saveCombo: %v", err)
	}
	for _, name := range []string{"models.txt", "combo.txt"} {
		if _, err := os.Stat(filepath.Join(cache, name)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked into cache root: %v", name, err)
		}
		if st, err := os.Stat(filepath.Join(root, name)); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s missing from state root: %v", name, err)
		}
	}
}
