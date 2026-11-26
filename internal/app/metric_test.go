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
