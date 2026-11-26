package app

import (
	"os"
	"path/filepath"
	"testing"
)

// F2: Flide onboarding stays loopback-only and needs no secret or new client.
func TestFlideLocalLoopbackOnboarding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WALSY_SO_MODE", "advisory")

	var flide provider
	for _, p := range bakedProviders {
		if p.Name == "Flide Local" {
			flide = p
			break
		}
	}
	if flide.Name == "" {
		t.Fatal("Flide Local baked slot missing")
	}
	if !isLoopbackBase(flide.Base) {
		t.Fatalf("Flide baked base is not loopback: %q", flide.Base)
	}
	if flide.KeyEnv != "" {
		t.Fatalf("Flide baked slot requires a key env: %q", flide.KeyEnv)
	}

	reg, err := loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range reg {
		if p.Name == flide.Name {
			found = true
			if p.Base != flide.Base {
				t.Fatalf("loaded Flide base = %q, want %q", p.Base, flide.Base)
			}
		}
	}
	if !found {
		t.Fatal("Flide Local absent from registry loaded from temp HOME")
	}

	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte("Flide Local\tjev-latest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	steps, err := loadCombo()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Provider != "Flide Local" || steps[0].Model != "jev-latest" {
		t.Fatalf("combo steps = %+v, want Flide Local/jev-latest", steps)
	}
	if !bakedProviderBase(flide.Base) {
		t.Fatalf("Flide base is not baked-equivalent for enforcing: %q", flide.Base)
	}

	t.Setenv("WALSY_SO_MODE", "enforcing")
	customPort := provider{Name: flide.Name, Base: "http://127.0.0.1:9123/v1/systemone", KeyEnv: "API_KEY"}
	if soProviderOverrideDeniedLine(customPort) {
		t.Fatal("loopback Flide port change denied in enforcing")
	}
	remote := provider{Name: flide.Name, Base: "https://flide.example/v1/systemone", KeyEnv: "API_KEY"}
	if !soProviderOverrideDeniedLine(remote) {
		t.Fatal("remote Flide override allowed in enforcing")
	}
	if baseRejectReason("http://flide.example/v1/systemone") == "" {
		t.Fatal("non-loopback plaintext Flide base accepted")
	}
	for _, p := range bakedProviders {
		if p.Name == flide.Name && !isLoopbackBase(p.Base) {
			t.Fatalf("non-loopback Flide baked: %q", p.Base)
		}
	}
}

// F2: a local Flide port change must still be usable when SO mode is enforcing.
func TestSOPoolEnforcingAcceptsLoopbackPortChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "enforcing")
	orig := soEndpoint
	soEndpoint = soEndpointDefaultBase
	t.Cleanup(func() { soEndpoint = orig })

	custom := "http://127.0.0.1:9123/v1/systemone"
	if err := writeRegistryLines([]provider{{
		Name: "Flide Local", Base: custom, KeyEnv: "FLIDE_API_KEY",
	}}); err != nil {
		t.Fatal(err)
	}
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"),
		[]byte("Flide Local\tjev-latest\n"), 0600); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, c := range soPool() {
		if c.Name != "Flide Local" {
			continue
		}
		found = true
		if c.Base != custom || c.Key != "" {
			t.Fatalf("enforcing Flide candidate = %+v, want keyless %s", c, custom)
		}
	}
	if !found {
		t.Fatal("enforcing mode dropped the loopback Flide port change")
	}
}
