package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	cases := []struct {
		f layer0
		w bool
	}{
		{layer0{true, true, true, true, true}, true},
		{layer0{false, true, true, true, true}, false},
		{layer0{true, false, true, true, true}, false},
		{layer0{true, true, false, true, true}, false},
		{layer0{true, true, true, true, false}, false},
		{layer0{CleanRename: true}, false},
	}
	for _, c := range cases {
		if got := gate(c.f); got != c.w {
			t.Fatalf("gate(%+v)=%v want %v", c.f, got, c.w)
		}
	}
}

func TestAskSOHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Error("missing x-api-key")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answers":{"gate":{"choice":"yes","confidence":0.95}}}`))
	}))
	defer srv.Close()
	orig := soEndpoint
	soEndpoint = srv.URL
	defer func() { soEndpoint = orig }()
	t.Setenv("EVREN_API_KEY", "test-key")

	d := askSO("user request only")
	if d.Choice != "yes" || d.Conf != 0.95 || d.TimedOut {
		t.Fatalf("unexpected decision: %+v", d)
	}
}

func TestAskSOFailurePassthrough(t *testing.T) {
	t.Setenv("EVREN_API_KEY", "test-key")
	orig := soEndpoint
	defer func() { soEndpoint = orig }()

	// HTTP 500 -> no decision, never an error surfaced to caller.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	soEndpoint = srv.URL
	if d := askSO("x"); d.Choice != "" || d.Conf != 0 {
		t.Fatalf("500 should be empty decision, got %+v", d)
	}
	srv.Close()

	// Low confidence -> degraded passthrough.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"choice":"yes","confidence":0.79}}}`))
	}))
	soEndpoint = srv2.URL
	if d := askSO("x"); d.Choice != "yes" || d.Conf != 0.79 {
		t.Fatalf("parser keeps the below-cut label for the caller to steer, got %+v", d)
	}
	srv2.Close()

	// Timeout -> TimedOut set, still empty decision.
	origTO := soTimeout
	soTimeout = 20 * time.Millisecond
	defer func() { soTimeout = origTO }()
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	soEndpoint = srv3.URL
	if d := askSO("x"); !d.TimedOut || d.Choice != "" {
		t.Fatalf("timeout should be TimedOut+empty, got %+v", d)
	}
	srv3.Close()
}

func TestPerItemThreshold(t *testing.T) {
	global := defaultThreshold()
	var it decisionItem
	for _, tc := range []struct{ thr, want float64 }{
		{0, global},
		{-1, global},
		{0.7, 0.7},
		{global, global},
		{0.95, global},
	} {
		it.Threshold = tc.thr
		if got := it.threshold(global); got != tc.want {
			t.Fatalf("threshold(%v)=%v want %v", tc.thr, got, tc.want)
		}
	}
}

// The measured 0.768 label the global gate discards is admitted under a
// below-global item cut, and an above-global cut clamps instead of loosening.
func TestPerItemThresholdReachesParser(t *testing.T) {
	q := soQuestion{Type: "choice", Criteria: map[string]string{"yes": "admit", "no": "pass"}}
	withSO(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"gate":{"choice":"yes","confidence":0.768}}}`))
	})
	if d := askSOQWithThreshold("decide: classify this", q, 0.7); d.Choice != "yes" || d.Conf != 0.768 {
		t.Fatalf("below-global cut must admit 0.768: %+v", d)
	}
	// Below the caller's cut the label is still returned, so the refusal can
	// steer; the cut is applied by route/toolsDecide, not by the parser.
	if d := askSOQWithThreshold("decide: classify this", q, 0.95); d.Choice != "yes" || d.Conf != 0.768 {
		t.Fatalf("parser must keep a below-cut label: %+v", d)
	}
}

// TestAskSOStoredProviderKey proves the SO key comes from any registry provider
// whose base matches the SO endpoint, not only from one hard-coded env name.
func TestAskSOStoredProviderKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "stored-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"answers":{"gate":{"choice":"yes","confidence":0.95}}}`))
	}))
	defer srv.Close()

	orig := soEndpoint
	soEndpoint = srv.URL
	defer func() { soEndpoint = orig }()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EVREN_API_KEY", "")
	state := filepath.Join(home, ".walsy-system1")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	writeProviders(t, state, "LOCAL-SO,"+srv.URL+",LOCAL_KEY\n", 0600)
	if err := saveKey("LOCAL-SO", "stored-key"); err != nil {
		t.Fatal(err)
	}

	d := askSO("state")
	if d.Choice != "yes" || d.Conf < 0.95 {
		t.Fatalf("stored provider key not used: %+v", d)
	}
}
