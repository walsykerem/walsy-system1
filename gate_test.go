package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	cases := []struct {
		f layer0
		w bool
	}{
		{layer0{true, true, true, true}, true},
		{layer0{false, true, true, true}, false},
		{layer0{true, false, true, true}, false},
		{layer0{true, true, false, true}, false},
		{layer0{true, true, true, false}, false},
		{layer0{}, false},
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
	if d := askSO("x"); d.Choice != "" {
		t.Fatalf("conf<0.8 should pass through, got %+v", d)
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
