package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestConfBucket(t *testing.T) {
	cases := []struct {
		c float64
		w string
	}{
		{0.79, ""},
		{0.85, "0.8"},
		{0.95, "0.9"},
		{1.0, "1.0"},
	}
	for _, k := range cases {
		if got := confBucket(k.c); got != k.w {
			t.Fatalf("confBucket(%v)=%q want %q", k.c, got, k.w)
		}
	}
}

func TestEmitMetricAllowlist(t *testing.T) {
	var buf bytes.Buffer
	orig := metricOut
	metricOut = &buf
	defer func() { metricOut = orig }()

	emitMetric(metric{Seq: 1, PID: 2, Decision: "passthrough", ConfBucket: "0.9"})
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, buf.String())
	}
	if got["conf_bucket"] != "0.9" || got["decision"] != "passthrough" {
		t.Fatalf("unexpected record: %v", got)
	}
	if _, ok := got["conf"]; ok {
		t.Fatal("raw conf must not be logged")
	}
}
