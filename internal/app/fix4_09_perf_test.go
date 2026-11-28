package app

import (
	"encoding/json"
	"fmt"
	"testing"
)

// fix4-09: permanent Go/stdlib performance harness. It holds the measured
// hotspots and a self-check of the measurement itself; no port ships.
const (
	fix4PerfRequest = `{"state":"classify the intent of: add a retry loop","item":"intake.intent","need_label":true,"no_scratch":true,"safe":true,"reversible":true,"authority":true,"calibrated":true,"phase":"before-model"}`
	fix4PerfAlias   = "\u0130stanbul STRASSE stra\u00DFe \u011E\u00DCN\u00DC\u00DCn \u00C7\u0130\u00C7EKLER mixed-W\u00D6RTER 12345"
)

var fix4PerfSink string

func benchFix4FoldAlias(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fix4PerfSink = foldAlias(fix4PerfAlias)
	}
}

func benchFix4SOState(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fix4PerfSink = buildSOState(fix4PerfRequest)
	}
}

func benchFix4SOBody(b *testing.B) {
	state := buildSOState(fix4PerfRequest)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, _ := json.Marshal(stateEnvelope{State: state, NeedLabel: true, NoScratch: true,
			Safe: true, Reversible: true, OptIn: true, Phase: "before-model",
			Authority: true, Calibrated: true, Item: "intake.intent"})
		fix4PerfSink = string(out)
	}
}

var fix4PerfCases = []struct {
	name string
	fn   func(*testing.B)
}{
	{"fold_alias", benchFix4FoldAlias},
	{"so_state", benchFix4SOState},
	{"so_request_body", benchFix4SOBody},
}

func BenchmarkFix4_09Hotspots(b *testing.B) {
	for _, c := range fix4PerfCases {
		b.Run(c.name, c.fn)
	}
}

// TestFix4_09PerfSelfCheck verifies the measurement itself: deterministic
// outputs and non-degenerate benchmark results, and reports us/op + alloc/op.
func TestFix4_09PerfSelfCheck(t *testing.T) {
	if got, want := foldAlias(fix4PerfAlias), foldAlias(fix4PerfAlias); got != want || got == "" {
		t.Fatalf("foldAlias nondeterministic or empty: %q / %q", got, want)
	}
	if got, want := buildSOState(fix4PerfRequest), buildSOState(fix4PerfRequest); got != want || got == "" {
		t.Fatalf("buildSOState nondeterministic or empty: %q / %q", got, want)
	}
	for _, c := range fix4PerfCases {
		r := testing.Benchmark(c.fn)
		if r.N <= 0 || r.T <= 0 {
			t.Fatalf("%s: degenerate measurement N=%d T=%s", c.name, r.N, r.T)
		}
		us := float64(r.T.Nanoseconds()) / float64(r.N) / 1000
		fmt.Printf("fix4-09 %s: N=%d us/op=%.3f alloc/op=%d B/op=%d total=%s\n",
			c.name, r.N, us, r.AllocsPerOp(), r.AllocedBytesPerOp(), r.T)
	}
}
