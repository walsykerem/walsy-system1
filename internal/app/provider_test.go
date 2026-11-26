package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func check(t *testing.T, cond bool, msg string) {
	t.Helper()
	if !cond {
		t.Fatal(msg)
	}
}

func writeProviders(t *testing.T, dir, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "providers.txt")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRegistryMultipleAndOverride(t *testing.T) {
	path := writeProviders(t, t.TempDir(),
		"Custom Ten, http://127.0.0.1:9999, CUSTOM_KEY\n"+
			"Evren System One, https://example.com/v1, OTHER_KEY\n"+
			"Second, https://second.example/v1, SECOND_KEY\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "multi-line registry accepted")
	check(t, len(got) == len(bakedProviders)+2, "baked + 2 new; override replaces in place")
	evren := -1
	for i, p := range got {
		if p.Name == "Evren System One" {
			evren = i
		}
	}
	check(t, evren >= 0, "baked override keeps its slot")
	check(t, got[evren].Base == "https://example.com/v1", "baked endpoint overridden")
	check(t, got[evren].KeyEnv == "OTHER_KEY", "baked key env overridden")
	check(t, got[len(bakedProviders)].Name == "Custom Ten" && got[len(bakedProviders)+1].Name == "Second", "new names append in order")
}

func TestLoadRegistryDuplicateLastWins(t *testing.T) {
	path := writeProviders(t, t.TempDir(),
		"X, http://127.0.0.1:1, K1\nX, http://127.0.0.1:2, K2\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "duplicate names accepted")
	check(t, len(got) == len(bakedProviders)+1, "duplicate collapses to one slot")
	check(t, got[len(bakedProviders)].Base == "http://127.0.0.1:2" && got[len(bakedProviders)].KeyEnv == "K2", "last line wins")
}

func TestLoadRegistryRejectsBadLineAndInjection(t *testing.T) {
	cases := []struct{ name, content string }{
		{"bad base on later line", "Good, http://127.0.0.1:1, K\nBad, http://example.com, K\n"},
		{"CR injection", "Good, http://127.0.0.1:1, K\nNa\rme, http://127.0.0.1:2, K\n"},
		{"demo name", "DEMO/OK, http://127.0.0.1:1, K\n"},
		{"malformed later line", "Good, http://127.0.0.1:1, K\nmalformed\n"},
		{"empty later field", "Good, http://127.0.0.1:1, K\nBad, http://127.0.0.1:2, \n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeProviders(t, t.TempDir(), tc.content, 0600)
			_, err := loadRegistryFrom(path)
			check(t, err != nil, "line must be rejected")
		})
	}
}

func TestKeyEnvSeparateField(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "X, http://[::1]:1234, MY_SECRET_ENV\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "ipv6 loopback accepted")
	last := got[len(got)-1]
	check(t, last.KeyEnv == "MY_SECRET_ENV", "third field is env name")
	check(t, !strings.Contains(last.Base, "MY_SECRET_ENV"), "key env name not in Base")
	data, _ := os.ReadFile(path)
	check(t, strings.Contains(string(data), "MY_SECRET_ENV"), "env NAME is in file (value is not)")
}

// (e) symlink rejection.
func TestSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	real := writeProviders(t, dir, "X, http://127.0.0.1:1, K\n", 0600)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable:", err)
	}
	_, err := loadRegistryFrom(link)
	check(t, err != nil, "symlink must be rejected")
}

// (g) non-0600 mode rejection.
func TestModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "X, http://127.0.0.1:1, K\n", 0644)
	_, err := loadRegistryFrom(path)
	check(t, err != nil, "0644 must be rejected")
	path2 := writeProviders(t, t.TempDir(), "X, http://127.0.0.1:1, K\n", 0600)
	_, err = loadRegistryFrom(path2)
	check(t, err == nil, "0600 accepted")
}

func TestRegistryRejectsOverlongProviderName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	long := strings.Repeat("a", maxProviderNameLen+1)
	if err := writeRegistryLines([]provider{{Name: long, Base: "http://127.0.0.1:1", KeyEnv: "K"}}); err == nil {
		t.Fatal("overlong name accepted")
	}
	if _, err := os.Stat(filepath.Join(home, ".walsy-system1", "providers.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed write created providers.txt: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".walsy-system1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".providers-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	exact := strings.Repeat("b", maxProviderNameLen)
	if err := writeRegistryLines([]provider{{Name: exact, Base: "http://127.0.0.1:1", KeyEnv: "K"}}); err != nil {
		t.Fatalf("exact-limit name rejected: %v", err)
	}
}

// A refused base must name its reason: the sheet prints it, so a silent snap
// back to the old endpoint is impossible.
func TestBaseRejectReasonNamesTheProblem(t *testing.T) {
	cases := []struct {
		base   string
		wantOK bool
		wantIn string
	}{
		{"https://api.example.com/v1", true, ""},
		{"https://user:pw@api.example.com/v1", false, "CREDENTIALS"},
		{"http://127.0.0.1:11434/v1/systemone", true, ""},
		{"http://localhost:8080/v1", true, ""},
		{"http://[::1]:8080/v1", true, ""},
		{"http://192.168.1.20:8080/v1", false, "127.0.0.1"},
		{"api.example.com/v1", false, "FULL URL"},
		{"ftp://example.com/v1", false, "http OR https"},
	}
	for _, tc := range cases {
		reason := baseRejectReason(tc.base)
		if tc.wantOK && reason != "" {
			t.Fatalf("%q rejected with %q, want accepted", tc.base, reason)
		}
		if !tc.wantOK && !strings.Contains(reason, tc.wantIn) {
			t.Fatalf("%q reason = %q, want it to mention %q", tc.base, reason, tc.wantIn)
		}
	}
}
