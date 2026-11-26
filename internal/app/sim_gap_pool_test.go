package app

// Simulation-only coverage for two decision-pool paths the existing tests do
// not drive: failover past a 200 response that carries no usable answer (an
// HTTP-500 failover already exists), and an enforcing pool with zero eligible
// candidates, which must abstain without sending anything to a custom base.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func simGapPoolEnv(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_SO_MODE", mode)
	t.Setenv("WALSY_SO_ENDPOINT", "")
	t.Setenv("EVREN_API_KEY", "") // no backstop: the combo is the whole pool
	routerReset()
	t.Cleanup(routerReset)
	orig := soEndpoint
	soEndpoint = soEndpointDefaultBase // only the default endpoint grows the pool
	t.Cleanup(func() { soEndpoint = orig })
}

func simGapPoolCombo(t *testing.T, providers []provider, combo string) {
	t.Helper()
	if err := writeRegistryLines(providers); err != nil {
		t.Fatal(err)
	}
	dir, err := stateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "combo.txt"), []byte(combo), 0600); err != nil {
		t.Fatal(err)
	}
}

// A 200 with an unusable answer, and a per-candidate timeout, must both fail
// over to the next pool candidate inside the shared deadline.
func TestSimGapPoolFailoverUnusableAnswer(t *testing.T) {
	t.Run("empty-answers", func(t *testing.T) {
		simGapPoolEnv(t, "advisory")
		var mu sync.Mutex
		var order []string
		var badHits, goodHits int
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			badHits++
			order = append(order, "bad")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"answers":{}}`))
		}))
		defer bad.Close()
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			goodHits++
			order = append(order, "good")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`))
		}))
		defer good.Close()

		simGapPoolCombo(t,
			[]provider{
				{Name: "BadSrv", Base: bad.URL + "/v1/systemone", KeyEnv: "GAP_BAD_KEY"},
				{Name: "GoodSrv", Base: good.URL + "/v1/systemone", KeyEnv: "GAP_GOOD_KEY"},
			},
			"BadSrv\tmodel-bad\nGoodSrv\tmodel-good\n")

		d := askSO("classify this request")
		if d.Choice != "yes" || d.Conf != 0.95 {
			t.Fatalf("no failover past an empty answer: %+v", d)
		}
		if badHits != 1 || goodHits != 1 {
			t.Fatalf("candidate hits bad/good = %d/%d, want 1/1", badHits, goodHits)
		}
		if len(order) != 2 || order[0] != "bad" || order[1] != "good" {
			t.Fatalf("pool order = %v, want bad then good", order)
		}
	})

	t.Run("candidate-timeout", func(t *testing.T) {
		simGapPoolEnv(t, "advisory")
		oldTimeout := soTimeout
		soTimeout = 600 * time.Millisecond
		t.Cleanup(func() { soTimeout = oldTimeout })

		var mu sync.Mutex
		var slowHits, goodHits int
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			slowHits++
			mu.Unlock()
			time.Sleep(800 * time.Millisecond)
			_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"no","confidence":0.95}}}`))
		}))
		defer slow.Close()
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			goodHits++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9}}}`))
		}))
		defer good.Close()

		simGapPoolCombo(t,
			[]provider{
				{Name: "SlowSrv", Base: slow.URL + "/v1/systemone", KeyEnv: "GAP_SLOW_KEY"},
				{Name: "FastSrv", Base: good.URL + "/v1/systemone", KeyEnv: "GAP_FAST_KEY"},
			},
			"SlowSrv\tmodel-slow\nFastSrv\tmodel-fast\n")

		d := askSO("classify this request")
		if d.Choice != "yes" || d.TimedOut {
			t.Fatalf("no failover past a candidate timeout: %+v", d)
		}
		if slowHits != 1 || goodHits != 1 {
			t.Fatalf("candidate hits slow/good = %d/%d, want 1/1", slowHits, goodHits)
		}
	})
}

// Enforcing mode must not send anything to an unbaked custom base; with no
// eligible candidate left the request fails closed instead of passing.
func TestSimGapPoolEnforcingExhaustedFailClosed(t *testing.T) {
	simGapPoolEnv(t, soModeEnforcing)
	t.Setenv("ROGUE_KEY", "rogue-secret")

	var rogueKey string
	var rogueHits int
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rogueHits++
		rogueKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.99}}}`))
	}))
	defer rogue.Close()

	// The httptest base is loopback, so a name-based unbaked slot is the only
	// way it could enter an enforcing pool; it must still be dropped.
	simGapPoolCombo(t,
		[]provider{{Name: "Rogue", Base: rogue.URL + "/v1/systemone", KeyEnv: "ROGUE_KEY"}},
		"Rogue\tmodel-rogue\n")

	for _, c := range soPool() {
		if !bakedEquivalentProvider(provider{Name: c.Name, Base: c.Base}) {
			t.Fatalf("enforcing pool kept an unbaked candidate: %+v", c)
		}
	}

	r := route(scopeState("classify this request", true), nil)
	if r.Decision != decDeny || r.Reason != "so-degraded" || !r.Abstained || r.Passthrough {
		t.Fatalf("exhausted enforcing pool did not fail closed: %+v", r)
	}
	if rogueHits != 0 || rogueKey != "" {
		t.Fatalf("custom base contacted (%d hits, key %q) in enforcing mode", rogueHits, rogueKey)
	}

	// Control: the same frame with an eligible baked-equivalent loopback slot
	// does reach a stub, so the empty pool above is the enforcement, not the
	// frame shape.
	t.Setenv("WALSY_SO_MODE", "advisory")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.95}}}`)
	}))
	defer ok.Close()
	simGapPoolCombo(t,
		[]provider{{Name: "Flide Local", Base: ok.URL + "/v1/systemone", KeyEnv: "GAP_OK_KEY"}},
		"Flide Local\tmodel-flide\n")
	if d := route(scopeState("classify this request", true), nil); d.Decision != decPin {
		t.Fatalf("control frame did not reach the loopback candidate: %+v", d)
	}
}
