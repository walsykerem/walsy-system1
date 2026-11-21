package main

import (
	"math"
	"testing"
)

// Synthetic under-confident data: labels systematically above scores, monotone.
func calibrationData() (scores, labels []float64) {
	for i := 0; i < 100; i++ {
		s := 0.2 + 0.79*float64(i)/99
		l := s * 1.25
		if l > 1 {
			l = 1
		}
		scores = append(scores, s)
		labels = append(labels, l)
	}
	return
}

func TestIsotonicReducesECE(t *testing.T) {
	scores, labels := calibrationData()
	raw := ece(scores, labels)
	fit := isotonicFit(scores, labels)
	cal := ece(fit, labels)
	if !(cal < raw) {
		t.Fatalf("ECE not reduced: raw=%v cal=%v", raw, cal)
	}
}

func TestQuantileThreshold(t *testing.T) {
	scores := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}
	// strong 0.7 -> q=0.3 -> nearest-rank index 2 -> 0.3
	if got := quantileThreshold(scores, 0.7); math.Abs(got-0.3) > 1e-9 {
		t.Fatalf("quantileThreshold(0.7)=%v want 0.3", got)
	}
	if got := quantileThreshold(nil, 0.7); got != defaultThreshold() {
		t.Fatalf("empty scores should fall back to default, got %v", got)
	}
}

// Parse failure pins to the strong model: absence != low confidence.
func TestParseFailurePinsStrong(t *testing.T) {
	if got := calibratedConfidence(0, false); got != 1 {
		t.Fatalf("parse failure must pin to 1, got %v", got)
	}
	if got := calibratedConfidence(0.9, true); got != 0.9 {
		t.Fatalf("valid parse must pass through, got %v", got)
	}
}

func TestRZeroAndDefault(t *testing.T) {
	// Monotone costs [0,1,2,3]: hull area = (0+1)/2+(1+2)/2+(2+3)/2 = 4.5, /3 = 1.5
	if got := rZero([]float64{0, 1, 2, 3}); math.Abs(got-1.5) > 1e-9 {
		t.Fatalf("rZero=%v want 1.5", got)
	}
	if got := rZero(nil); got != 0.8 {
		t.Fatalf("rZero insufficient data fallback=%v want 0.8", got)
	}
	if defaultThreshold() != 0.8 {
		t.Fatalf("defaultThreshold()=%v want 0.8", defaultThreshold())
	}
}
