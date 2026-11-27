package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradePackageVersionCleanup(t *testing.T) {
	seed := func(t *testing.T, installed string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(installed+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "pkg-"+installed), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pkg-"+installed, "old.txt"), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	installedVersion := func(t *testing.T, dir string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "VERSION"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	exists := func(t *testing.T, path string) bool {
		t.Helper()
		_, err := os.Stat(path)
		if err == nil {
			return true
		}
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		t.Fatal(err)
		return false
	}
	install := func(staging string) error {
		return os.WriteFile(filepath.Join(staging, "current.txt"), []byte("current"), 0600)
	}

	t.Run("comparator matches repo tags", func(t *testing.T) {
		for _, tc := range []struct {
			older, newer string
			want         bool
		}{
			{"0.1.0-fix-9", "0.1.0-fix-10", true},
			{"1.0", "1.1e", true},
			{"1.1e", "1.1.0", false},
			{"1.1.0", "1.0.9", false},
		} {
			a, okA := parseVersion(tc.older)
			b, okB := parseVersion(tc.newer)
			if !okA || !okB {
				t.Fatalf("parse %q/%q: %v/%v", tc.older, tc.newer, okA, okB)
			}
			if got := versionLess(a, b); got != tc.want {
				t.Fatalf("versionLess(%q,%q) = %v, want %v", tc.older, tc.newer, got, tc.want)
			}
		}
	})

	t.Run("upgrade removes stale package", func(t *testing.T) {
		dir := seed(t, "1.0")
		if err := upgradePackage(dir, "1.0", "1.1e", install); err != nil {
			t.Fatal(err)
		}
		if got := installedVersion(t, dir); got != "1.1e" {
			t.Fatalf("VERSION = %q, want 1.1e", got)
		}
		if !exists(t, filepath.Join(dir, "pkg-1.1e", "current.txt")) {
			t.Fatal("current package not installed")
		}
		if exists(t, filepath.Join(dir, "pkg-1.0")) {
			t.Fatal("stale 1.0 package left behind")
		}
	})

	t.Run("below threshold leaves old state", func(t *testing.T) {
		dir := seed(t, "1.0")
		called := false
		err := upgradePackage(dir, "1.0", "1.0.9", func(staging string) error {
			called = true
			return install(staging)
		})
		if err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("install called below cleanup floor")
		}
		if got := installedVersion(t, dir); got != "1.0" {
			t.Fatalf("VERSION = %q, want 1.0", got)
		}
		if !exists(t, filepath.Join(dir, "pkg-1.0", "old.txt")) {
			t.Fatal("old package changed below cleanup floor")
		}
	})

	t.Run("repeating upgrade is idempotent", func(t *testing.T) {
		dir := seed(t, "1.0")
		if err := upgradePackage(dir, "1.0", "1.1e", install); err != nil {
			t.Fatal(err)
		}
		calls := 0
		err := upgradePackage(dir, installedVersion(t, dir), "1.1e", func(staging string) error {
			calls++
			return install(staging)
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("second upgrade called install %d times", calls)
		}
		if got := installedVersion(t, dir); got != "1.1e" {
			t.Fatalf("VERSION = %q, want 1.1e", got)
		}
		if !exists(t, filepath.Join(dir, "pkg-1.1e", "current.txt")) || exists(t, filepath.Join(dir, "pkg-1.0")) {
			t.Fatal("repeated upgrade changed the installed state")
		}
	})

	t.Run("failed install keeps state consistent", func(t *testing.T) {
		dir := seed(t, "1.0")
		wantErr := errors.New("install failed")
		err := upgradePackage(dir, "1.0", "1.1e", func(staging string) error {
			_ = os.WriteFile(filepath.Join(staging, "partial.txt"), []byte("x"), 0600)
			return wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if got := installedVersion(t, dir); got != "1.0" {
			t.Fatalf("VERSION = %q, want 1.0", got)
		}
		if !exists(t, filepath.Join(dir, "pkg-1.0", "old.txt")) {
			t.Fatal("old package lost after failed install")
		}
		if exists(t, filepath.Join(dir, "pkg-1.1e")) {
			t.Fatal("target package present after failed install")
		}
		if stages, err := filepath.Glob(filepath.Join(dir, ".stage-*")); err != nil {
			t.Fatal(err)
		} else if len(stages) != 0 {
			t.Fatalf("staging left behind: %v", stages)
		}
	})
}
