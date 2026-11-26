package app

import (
	"os"
	"testing"
)

// Tests must be hermetic: never inherit the machine's ~/.walsy-system1 config
// (e.g. an enforcing flip), which would change gate behavior under test. Tests
// that need a specific state still override HOME with t.Setenv.
func TestMain(m *testing.M) {
	// The suite cites repo-root paths (walsy-system1, scripts/, docs/, testdata/)
	// because it used to live at the root; keep those paths valid from here.
	if err := os.Chdir("../.."); err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "walsy-test-home-")
	if err != nil {
		panic(err)
	}
	old, had := os.LookupEnv("HOME")
	_ = os.Setenv("HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	if had {
		_ = os.Setenv("HOME", old)
	} else {
		_ = os.Unsetenv("HOME")
	}
	os.Exit(code)
}
