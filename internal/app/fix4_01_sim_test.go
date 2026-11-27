package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkFix4_01RouterRouteOrder(b *testing.B) {
	home, err := os.MkdirTemp("", "fix4-01-bench-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(home)
	oldHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", home)
	defer os.Setenv("HOME", oldHome)
	_ = os.MkdirAll(filepath.Join(home, routerDir), 0700)
	routerReset()
	defer routerReset()

	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	scores := make(map[string]float64, len(names))
	for i, n := range names {
		scores[n] = float64(i) / 8
		routerRecord(n, false)
	}
	routerRecord("a", true)
	routerRecord("b", true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := routerRouteOrder(names, scores); len(got) != len(names) {
			b.Fatalf("order length = %d", len(got))
		}
	}
}

func BenchmarkFix4_01DecisionWithRouteOrder(b *testing.B) {
	home, err := os.MkdirTemp("", "fix4-01-e2e-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(home)
	b.Setenv("HOME", home)
	b.Setenv("WALSY_SO_MODE", "advisory")
	b.Setenv("WALSY_SO_ENDPOINT", "")
	b.Setenv("EVREN_API_KEY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"gate":{"type":"choice","choice":"yes","confidence":0.9,"probabilities":{"yes":1,"no":0}}}}`))
	}))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	defer func() { soEndpoint = orig }()
	routerReset()
	defer routerReset()

	names := []string{"a", "b", "c", "d"}
	scores := map[string]float64{"a": 0.9, "b": 0.7, "c": 0.5, "d": 0.3}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = routerRouteOrder(names, scores)
		if d := askSO("classify this"); d.Choice != "yes" {
			b.Fatalf("decision = %+v", d)
		}
	}
}

func fix4_01Env(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WALSY_ROUTER_BREAKER_THRESHOLD", "2")
	t.Setenv("WALSY_ROUTER_BREAKER_COOLDOWN", "1h")
	t.Setenv("WALSY_ROUTER_LATENCY_THRESHOLD_MS", "1")
	routerReset()
	t.Cleanup(routerReset)
}

// A timed-out candidate must not outrank a healthy one even with a higher
// score. Equal scores keep caller order; repeats must be byte-identical.
func TestFix4_01SmartOrderDemotesDeadAndDeterministicTies(t *testing.T) {
	fix4_01Env(t)
	routerRecord("dead", false, 200*time.Millisecond)
	routerRecord("dead", false, 200*time.Millisecond)
	routerRecord("slow", true, 5*time.Millisecond)
	routerRecord("slow", true, 5*time.Millisecond)

	names := []string{"liveB", "dead", "liveA", "slow", "liveA"}
	scores := map[string]float64{"liveA": 0.5, "liveB": 0.5, "dead": 0.9, "slow": 0.8}
	want := []string{"liveB", "liveA", "dead", "slow"}
	for i := 0; i < 20; i++ {
		got := routerRouteOrder(names, scores)
		if len(got) != len(want) {
			t.Fatalf("order %d = %v, want %v", i, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("order %d = %v, want %v", i, got, want)
			}
		}
	}
}

// A fresh fast answer supersedes stale slow samples; otherwise a recovered
// candidate stays marked dead until the whole latency ring rotates.
func TestFix4_01FastSuccessClosesStaleLatencyCircuit(t *testing.T) {
	fix4_01Env(t)
	routerRecord("recovered", true, 5*time.Millisecond)
	routerRecord("recovered", true, 5*time.Millisecond)
	if !routerBreakerOpen("recovered") {
		t.Fatal("two slow samples must open the latency circuit")
	}
	routerRecord("recovered", true, 50*time.Microsecond)
	if routerBreakerOpen("recovered") {
		t.Fatal("fast success left stale slow samples in the circuit")
	}
}

// Loopback-only failover: a breaker-open candidate is demoted behind the live
// one even when its score is higher, and the live candidate wins after a retry.
func TestFix4_01FailoverUsesHealthyFirst(t *testing.T) {
	fix4_01Env(t)
	var deadHits, liveHits atomic.Int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if liveHits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer live.Close()

	routerRecord("fix4dead", false, 200*time.Millisecond)
	routerRecord("fix4dead", false, 200*time.Millisecond)
	order := routerRouteOrder([]string{"fix4dead", "fix4live"}, map[string]float64{
		"fix4dead": 0.9, "fix4live": 0.1,
	})
	bases := map[string]string{"fix4dead": dead.URL, "fix4live": live.URL}
	steps := make([]ComboStep, 0, len(order))
	for _, name := range order {
		steps = append(steps, ComboStep{Name: name, Base: bases[name]})
	}
	got := RunCombo(steps, 0)
	if got.Winner != "fix4live" {
		t.Fatalf("winner = %q, want fix4live: %+v", got.Winner, got)
	}
	if deadHits.Load() != 0 {
		t.Fatalf("breaker-open candidate received %d probes", deadHits.Load())
	}
	if liveHits.Load() < 2 {
		t.Fatalf("live candidate probes = %d, want failover retry", liveHits.Load())
	}
}
