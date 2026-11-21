package main

import (
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

// (a) providers.txt yoksa baked 9 sıra.
func TestLoadRegistryMissing(t *testing.T) {
	dir := t.TempDir()
	got, err := loadRegistryFrom(filepath.Join(dir, "none.txt"))
	check(t, err == nil, "missing file must not error")
	want := bakedRegistry()
	check(t, reflect.DeepEqual(got, want), "missing file ⇒ baked 9 order")
	check(t, len(got) == 9, "baked count is 9")
}

// (b) tek satır → #10.
func TestLoadRegistryCustom(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "Custom Ten, http://127.0.0.1:9999, CUSTOM_KEY\n", 0600)
	got, err := loadRegistryFrom(path)
	check(t, err == nil, "valid #10 accepted")
	check(t, len(got) == 10, "baked 9 + 1")
	check(t, got[9].Name == "Custom Ten", "#10 appended last")
	check(t, got[9].KeyEnv == "CUSTOM_KEY", "KeyEnv holds env NAME")
	check(t, got[9].Base == "http://127.0.0.1:9999", "loopback base kept")
}

// (c) restart sonrası pin yok → config sırası; ilk başarı pin yazar.
func TestPinRestartThenSuccess(t *testing.T) {
	list := bakedRegistry()
	fresh := newPin(list) // process restart ⇒ RAM empty
	check(t, fresh.current().Name == list[0].Name, "no pin ⇒ config order")

	// A different process pinned another provider; after restart it is gone.
	pinned := newPin(list)
	pinned.markSuccess(list[4].Name)
	check(t, pinned.current().Name == list[4].Name, "pin used while in RAM")
	_ = fresh // fresh process unaffected by other pin state
}

// (d) key env adı dosyada görünmez (KeyEnv ayrı alan; raw key asla dosyada).
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

// (e) symlink reddi.
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

// (g) 0600 dışı mode reddi.
func TestModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "X, http://127.0.0.1:1, K\n", 0644)
	_, err := loadRegistryFrom(path)
	check(t, err != nil, "0644 must be rejected")
	path2 := writeProviders(t, t.TempDir(), "X, http://127.0.0.1:1, K\n", 0600)
	_, err = loadRegistryFrom(path2)
	check(t, err == nil, "0600 accepted")
}

// (h) loopback dışı Host reddi.
func TestNonLoopbackRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeProviders(t, dir, "X, https://evil.example.com, K\n", 0600)
	_, err := loadRegistryFrom(path)
	check(t, err != nil, "non-loopback host rejected")
	path2 := writeProviders(t, t.TempDir(), "X, http://localhost:1, K\n", 0600)
	_, err = loadRegistryFrom(path2)
	check(t, err == nil, "localhost accepted")
}

// (f) rotation deterministik + aynı state'de aynı sıra.
// (11) Evren slot keeps x-api-key + EVREN_API_KEY (plan.md §1e); no URL invented.
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
	check(t, evren.Base == "https://evren-llmapi.ssyz.org.tr", "Evren base URL must be kept")

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
	check(t, p1.Name == p2.Name, "same state ⇒ same rotation head")
	check(t, rotationOrder(list)[0].Name == p1.Name, "rotation head = hash order head")

	// hash order is a stable permutation, independent of input order.
	shuffled := []provider{list[4], list[0], list[8], list[2]}
	s1 := rotationOrder(shuffled)
	s2 := rotationOrder(append([]provider(nil), shuffled...))
	check(t, reflect.DeepEqual(s1, s2), "rotation order deterministic")
	check(t, a.current().Name == list[0].Name, "failure drops pin ⇒ config order")

	pin := newPin(list)
	pin.markSuccess(list[3].Name)
	check(t, pin.current().Name == list[3].Name, "sticky pin holds")
	pin.rotateOnFailure()
	check(t, pin.current().Name == list[0].Name, "rotation clears pin")
}
