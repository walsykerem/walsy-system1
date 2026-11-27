package app

// Simulation: the first pool candidate times out and the second answers with a
// typed rejection. The timeout must survive into the reported decision, or SO
// health (routerRecord) and the latency budget both read a dead oracle as a
// healthy one.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sim03Pool(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", "advisory")
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "") // no backstop: the combo is the whole pool
	routerReset()
	t.Cleanup(routerReset)
	orig := soEndpoint
	soEndpoint = soEndpointDefaultBase // only the default endpoint grows the pool
	t.Cleanup(func() { soEndpoint = orig })
}

func TestSim03TimeoutSurvivesTypedRejection(t *testing.T) {
	sim03Pool(t)
	oldTimeout := soTimeout
	soTimeout = 400 * time.Millisecond
	t.Cleanup(func() { soTimeout = oldTimeout })

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9}}}`))
	}))
	defer slow.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9,"probabilities":{"yes":1}}}}`))
	}))
	defer bad.Close()

	if err := writeRegistryLines([]provider{
		{Name: "Sim03Slow", Base: slow.URL + "/v1/systemone", KeyEnv: "SIM03_SLOW_KEY"},
		{Name: "Sim03Bad", Base: bad.URL + "/v1/systemone", KeyEnv: "SIM03_BAD_KEY"},
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	combo := "Sim03Slow\tslow-model\nSim03Bad\tbad-model\n"
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte(combo), 0600); err != nil {
		t.Fatal(err)
	}

	d := askSO("classify this request")
	if d.Choice != "" {
		t.Fatalf("failed pool returned a choice: %+v", d)
	}
	if d.Reason != "invalid_probabilities" {
		t.Fatalf("reason = %q, want invalid_probabilities", d.Reason)
	}
	if !d.TimedOut {
		t.Fatalf("timeout from the first candidate was dropped: %+v", d)
	}
}
