package app

import (
	"os"
	"path/filepath"
	"testing"
)

// fix417Home installs a hermetic HOME with one config.txt body. Env overrides
// are cleared so the file is the only mode source under test.
func fix417Home(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "")
	t.Setenv("WALSY_SO_APPROVER", "")
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A malformed, misspelled or duplicated security key must never turn an
// intended enforcing setting into advisory. Valid CRLF config stays advisory.
func TestFix417ConfigValidationFailClosed(t *testing.T) {
	cases := []struct {
		name, body, want string
		startRefusal     bool
	}{
		{"typed key", "so_mod=enforcing\n", soModeEnforcing, true},
		{"duplicate downgrade", "so_mode=enforcing\nso_mode=advisory\n", soModeEnforcing, true},
		{"BOM prefix", "\xef\xbb\xbfso_mode=enforcing\n", soModeEnforcing, true},
		{"broken line", "so_mode enforcing\n", soModeEnforcing, true},
		{"valid CRLF advisory", "so_mode=advisory\r\napprover=Op\r\n", soModeAdvisory, false},
		{"valid CRLF enforcing", "so_mode=enforcing\r\napprover=Op\r\n", soModeEnforcing, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fix417Home(t, c.body)
			if got := soMode(); got != c.want {
				t.Fatalf("soMode = %q, want %q", got, c.want)
			}
			err := soModeStartupError()
			if c.startRefusal {
				if err == nil {
					t.Fatal("startup accepted rejected config without a valid enforcing source")
				}
			} else if err != nil {
				t.Fatalf("startup refused valid config: %v", err)
			}
		})
	}
}

// Config is re-read on every call: a rejected file, a repaired file and an
// enforcing flip all take effect in one process, with no restart.
func TestFix417SOModeReload(t *testing.T) {
	path := fix417Home(t, "so_mod=enforcing\n")
	if got := soMode(); got != soModeEnforcing {
		t.Fatalf("rejected config mode = %q, want enforcing", got)
	}
	rewrite := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("so_mode=advisory\n")
	if got := soMode(); got != soModeAdvisory {
		t.Fatalf("repaired config mode = %q, want advisory", got)
	}
	rewrite("so_mode=enforcing\napprover=Op\n")
	if got := soMode(); got != soModeEnforcing {
		t.Fatalf("reloaded enforcing mode = %q, want enforcing", got)
	}
}

// Machine-independent cost of one config read, for the port/no-port decision.
func BenchmarkFix417SOConfigRead(b *testing.B) {
	fix417BenchConfig(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = soMode()
	}
}

// Parse-only cost, separated from the audit append that dominates soMode.
func BenchmarkFix417ConfigParse(b *testing.B) {
	fix417BenchConfig(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = readSOConfig(), soConfigSecurityError()
	}
}

// Baseline reader alone: the validator's added cost is the delta to ConfigParse.
func BenchmarkFix417ConfigBaseline(b *testing.B) {
	fix417BenchConfig(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = readSOConfig()
	}
}

func fix417BenchConfig(b *testing.B) {
	b.Helper()
	home := b.TempDir()
	b.Setenv("HOME", home)
	dir := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte("so_mode=advisory\r\napprover=Op\r\n"), 0600); err != nil {
		b.Fatal(err)
	}
}
