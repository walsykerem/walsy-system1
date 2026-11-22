package main

import (
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// benchSORequest is a concrete client frame: state text plus the opt-in fields
// buildSOState fills for a real client. No key/secret, no network.
const benchSORequest = `{"state":"classify the intent of: add a retry loop","item":"intake.intent","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"phase":"before-model"}`

// benchSORequestBody mirrors askSOQ's json.Marshal payload (gate.go:156-164),
// so this sub-benchmark measures the real SO request body cost.
func benchSORequestBody(state string) []byte {
	b, _ := json.Marshal(struct {
		Model     string                `json:"model"`
		State     string                `json:"state"`
		Questions map[string]soQuestion `json:"questions"`
	}{
		Model: "clef",
		State: state,
		Questions: map[string]soQuestion{"gate": {Type: "choice", Criteria: map[string]string{
			"yes":   "admit to the typed decision layer",
			"no":    "pass through unchanged",
			"other": "ambiguous; pass through",
		}}},
	})
	return b
}

// benchSamples keeps a bounded, deterministic sample of per-op timings so the
// slice itself does not skew -benchmem. Capacity is allocated once per sub-bench.
type benchSamples struct {
	buf    []float64
	stride int64
}

func newBenchSamples(n int) *benchSamples {
	const capSamples = 8192
	stride := int64(n / capSamples)
	if stride < 1 {
		stride = 1
	}
	return &benchSamples{buf: make([]float64, 0, capSamples), stride: stride}
}

func (s *benchSamples) add(i int, d time.Duration) {
	if int64(i)%s.stride == 0 && len(s.buf) < cap(s.buf) {
		s.buf = append(s.buf, float64(d.Nanoseconds()))
	}
}

func (s *benchSamples) report(b *testing.B) {
	b.Helper()
	if len(s.buf) == 0 {
		return
	}
	v := append([]float64(nil), s.buf...)
	sort.Float64s(v)
	at := func(q float64) float64 {
		return v[int(q*float64(len(v)-1))]
	}
	b.ReportMetric(at(0.50), "p50-ns/op")
	b.ReportMetric(at(0.95), "p95-ns/op")
}

var benchSink string

// BenchmarkSOStateBuild has three sub-benchmarks: state build, json.Marshal,
// and the full SO request body. Per-op p50/p95 ns/op come from the samples;
// B/op and allocs/op come from -benchmem.
func BenchmarkSOStateBuild(b *testing.B) {
	b.Run("state_build", func(b *testing.B) {
		s := newBenchSamples(b.N)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			t0 := time.Now()
			out := buildSOState(benchSORequest)
			s.add(i, time.Since(t0))
			benchSink = out
		}
		s.report(b)
	})
	b.Run("json_marshal", func(b *testing.B) {
		s := newBenchSamples(b.N)
		state := buildSOState(benchSORequest)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			t0 := time.Now()
			out, _ := json.Marshal(stateEnvelope{State: state, NeedLabel: true, NoScratch: true,
				Safe: true, Reversible: true, OptIn: true, Phase: "before-model",
				Authority: true, Calibrated: true, Item: "intake.intent"})
			s.add(i, time.Since(t0))
			benchSink = string(out)
		}
		s.report(b)
	})
	b.Run("request_body", func(b *testing.B) {
		s := newBenchSamples(b.N)
		state := buildSOState(benchSORequest)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			t0 := time.Now()
			out := benchSORequestBody(state)
			s.add(i, time.Since(t0))
			benchSink = string(out)
		}
		s.report(b)
	})
}
