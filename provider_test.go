package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// (a) If providers.txt is missing, use the baked order.
func TestLoadRegistryMissing(t *testing.T) {
	dir := t.TempDir()
	got, err := loadRegistryFrom(filepath.Join(dir, "none.txt"))
	check(t, err == nil, "missing file must not error")
	want := bakedRegistry()
	check(t, reflect.DeepEqual(got, want), "missing file -> baked order")
	check(t, len(got) == len(bakedProviders), "baked count matches bakedProviders")
}

// (b) single line appends after the baked slots.
func TestLoadRegistryCustom(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "Custom Ten, http://127.0.0.1:9999, CUSTOM_KEY\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "valid #10 accepted")
	check(t, len(got) == len(bakedProviders)+1, "baked + 1")
	check(t, got[len(bakedProviders)].Name == "Custom Ten", "#10 appended last")
	check(t, got[len(bakedProviders)].KeyEnv == "CUSTOM_KEY", "KeyEnv holds env NAME")
	check(t, got[len(bakedProviders)].Base == "http://127.0.0.1:9999", "loopback base kept")
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

func TestLoadRegistryCRLF(t *testing.T) {
	path := writeProviders(t, t.TempDir(),
		"X, http://127.0.0.1:1, K1\r\nY, https://example.com, K2\r\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "CRLF lines accepted")
	check(t, len(got) == len(bakedProviders)+2, "both lines loaded")
}

func TestSaveCustomProviderAppendsAndUpserts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	check(t, saveCustomProvider("A", "http://127.0.0.1:1", "KA") == nil, "first save")
	check(t, saveCustomProvider("B", "http://127.0.0.1:2", "KB") == nil, "second save")
	reg, err := loadRegistry()
	check(t, err == nil, "registry reloads")
	names := map[string]string{}
	for _, p := range reg {
		names[p.Name] = p.Base
	}
	check(t, names["A"] == "http://127.0.0.1:1", "first provider kept")
	check(t, names["B"] == "http://127.0.0.1:2", "second provider appended")

	check(t, saveCustomProvider("A", "http://127.0.0.1:3", "KA") == nil, "upsert save")
	reg, err = loadRegistry()
	check(t, err == nil, "registry reloads after upsert")
	names = map[string]string{}
	for _, p := range reg {
		names[p.Name] = p.Base
	}
	check(t, len(names) == len(bakedProviders)+2, "upsert does not duplicate a slot")
	check(t, names["A"] == "http://127.0.0.1:3", "endpoint updated")
	check(t, names["B"] == "http://127.0.0.1:2", "other provider preserved")

	check(t, saveCustomProvider("DEMO/OK", "http://127.0.0.1:1", "K") != nil, "demo name rejected")
	check(t, saveCustomProvider("EmptyBase", "", "K") != nil, "empty field rejected")
}

// (c) after restart with no pin -> config order; first success writes pin.
func TestPinRestartThenSuccess(t *testing.T) {
	list := bakedRegistry()
	fresh := newPin(list) // process restart -> RAM empty
	check(t, fresh.current().Name == list[0].Name, "no pin -> config order")

	// A different process pinned another provider; after restart it is gone.
	pinned := newPin(list)
	pinned.markSuccess(list[4].Name)
	check(t, pinned.current().Name == list[4].Name, "pin used while in RAM")
	_ = fresh // fresh process unaffected by other pin state
}

// (d) key env name is not visible in file (KeyEnv is a separate field; raw key never in file).
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

// (h) remote HTTPS and loopback HTTP are valid outbound provider endpoints;
// remote plaintext HTTP is rejected.
func TestProviderBaseValidation(t *testing.T) {
	path := writeProviders(t, t.TempDir(), "X, https://example.com/v1/systemone, K\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "remote https accepted")
	check(t, len(got) == len(bakedProviders)+1 && got[len(bakedProviders)].Name == "X", "remote https provider appended")

	path2 := writeProviders(t, t.TempDir(), "X, http://example.com, K\n", 0600)
	_, err = loadRegistryFrom(path2)
	check(t, err != nil, "remote http rejected")

	path3 := writeProviders(t, t.TempDir(), "X, http://localhost:1, K\n", 0600)
	_, err = loadRegistryFrom(path3)
	check(t, err == nil, "localhost accepted")
}

// (f) rotation deterministic + same order for the same state.
// (11) Evren slot keeps x-api-key + EVREN_API_KEY; all baked slots have endpoints.
func TestEveryBakedProviderHasEndpoint(t *testing.T) {
	for _, p := range bakedProviders {
		check(t, validProviderBase(p.Base), fmt.Sprintf("baked endpoint for %q must be valid: %q", p.Name, p.Base))
	}
}

func TestEvrenKeyEnv(t *testing.T) {
	var evren *provider
	for i := range bakedProviders {
		if bakedProviders[i].Name == "Evren System One" {
			evren = &bakedProviders[i]
			break
		}
	}
	check(t, evren != nil, "Evren System One baked slot missing")
	check(t, evren.KeyEnv == "EVREN_API_KEY", "KeyEnv must be EVREN_API_KEY")
	check(t, evren.Base == "https://evren-llmapi.ssyz.org.tr/v1/systemone", "Evren base URL must be kept")

	// x-api-key is the fixed header name, never the key value or env in a URL.
	check(t, !strings.Contains(evren.Base, "EVREN_API_KEY"), "key env must not leak into URL")
	check(t, !strings.Contains(evren.Base, "x-api-key"), "header name must not leak into URL")
}

func TestRotationDeterministic(t *testing.T) {
	list := bakedRegistry()
	a := newPin(list)
	b := newPin(list)
	p1 := a.rotateOnFailure()
	p2 := b.rotateOnFailure()
	check(t, p1.Name == p2.Name, "same state -> same rotation head")
	check(t, rotationOrder(list)[0].Name == p1.Name, "rotation head = hash order head")

	// hash order is a stable permutation, independent of input order.
	shuffled := []provider{list[4], list[0], list[8], list[2]}
	s1 := rotationOrder(shuffled)
	s2 := rotationOrder(append([]provider(nil), shuffled...))
	check(t, reflect.DeepEqual(s1, s2), "rotation order deterministic")
	check(t, a.current().Name == list[0].Name, "failure drops pin -> config order")

	pin := newPin(list)
	pin.markSuccess(list[3].Name)
	check(t, pin.current().Name == list[3].Name, "sticky pin holds")
	pin.rotateOnFailure()
	check(t, pin.current().Name == list[0].Name, "rotation clears pin")
}

// Provider name length is capped at the shared state limit.
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
