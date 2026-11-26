package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

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

// Upstream stderr shares the metric channel, so every line of a multi-line
// chunk must carry the prefix; otherwise a forged record looks like a metric.
func TestPrefixWriterTagsEveryLine(t *testing.T) {
	var buf bytes.Buffer
	p := &prefixWriter{w: &buf, prefix: []byte("upstream: ")}
	if _, err := p.Write([]byte("warn\n{\"seq\":1,\"pid\":1}\n")); err != nil {
		t.Fatal(err)
	}
	want := "upstream: warn\nupstream: {\"seq\":1,\"pid\":1}\n"
	if buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}
