package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R9: the decision gate walks the operator's combo in order, with each
// provider's own key and model, and fails over past a broken provider.
func TestRound9ComboDecisionFailover(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "backstop-key")
	t.Setenv("ROUND9_BAD_KEY", "bad-key")
	t.Setenv("ROUND9_GOOD_KEY", "good-key")

	badHits, goodHits := 0, 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits++
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got := r.Header.Get("x-api-key"); got != "" || body.Model != "model-good" {
			t.Errorf("candidate key/model = %q/%q, want no loopback key/model-good", got, body.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
	}))
	defer good.Close()

	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf("BadSrv,%s/v1/systemone,ROUND9_BAD_KEY\nGoodSrv,%s/v1/systemone,ROUND9_GOOD_KEY\n", bad.URL, good.URL)
	if err := os.WriteFile(filepath.Join(dir, "providers.txt"), []byte(reg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte("BadSrv\tmodel-bad\nGoodSrv\tmodel-good\n"), 0600); err != nil {
		t.Fatal(err)
	}

	old := soEndpoint
	soEndpoint = soEndpointDefaultBase // default endpoint is the only case where the pool grows
	defer func() { soEndpoint = old }()

	d := askSO("user request only")
	if d.Choice != "yes" || d.Conf != 0.95 {
		t.Fatalf("combo answer = %+v, want yes/0.95", d)
	}
	if badHits != 1 || goodHits != 1 {
		t.Fatalf("combo hits bad/good = %d/%d, want 1/1", badHits, goodHits)
	}
}

// R9: enforcing mode never sends a key to a base outside the baked pin, even
// when the operator adds it to providers.txt/combo.txt.
func TestRound9EnforcingSkipsUnvettedBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "enforcing")
	t.Setenv("EVREN_API_KEY", "backstop-key")

	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	reg := "Rogue,https://rogue.example/v1/systemone,ROGUE_KEY\n"
	if err := os.WriteFile(filepath.Join(dir, "providers.txt"), []byte(reg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte("Rogue\tmodel-rogue\n"), 0600); err != nil {
		t.Fatal(err)
	}

	old := soEndpoint
	soEndpoint = soEndpointDefaultBase
	defer func() { soEndpoint = old }()

	for _, c := range soPool() {
		if !bakedProviderBase(c.Base) {
			t.Fatalf("enforcing pool contains unvetted base %q", c.Base)
		}
		if strings.Contains(c.Key, "ROGUE") {
			t.Fatal("enforcing pool used the rogue provider key")
		}
	}
}

// R9: a cache hit carries the stored LLM label and steer without a second
// POST. Only the coarse bucket stays in the cell; the typed answer is rebuilt
// from it so tools on a cache hit keep the live-call shape.
func TestRound9CacheHitCarriesLLM(t *testing.T) {
	admit := routeResult{Decision: decPin, Reason: "so-admit", Covered: true,
		ConfBucket: "0.9", Label: "build", Steer: "continue",
		LastDecision: soDecision{Choice: "build", Conf: 0.95}}
	cell := cacheResult(admit)
	if cell.Label != "build" || cell.Steer != "continue" || cell.Choice != "yes" {
		t.Fatalf("cached cell = %+v, want the LLM label/steer", cell)
	}
	hit := cachedResult(cell)
	if hit.Reason != "cache-hit" || hit.Decision != decPin || hit.Label != "build" ||
		hit.Steer != "continue" || hit.ConfBucket != "0.9" || hit.LastDecision.Conf != 0.9 ||
		hit.LastDecision.Choice != "build" {
		t.Fatalf("cache hit = %+v, want label/steer and bucket-derived typed decision", hit)
	}
	// A generic admit has no registry label; the SO choice is the label.
	generic := cacheResult(routeResult{Decision: decPin, Reason: "so-admit", Covered: true,
		ConfBucket: "0.8", LastDecision: soDecision{Choice: "yes", Conf: 0.83}})
	if generic.Label != "yes" {
		t.Fatalf("generic cache label = %q, want yes", generic.Label)
	}
}
