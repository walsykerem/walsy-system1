package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// metricOut is the single stderr allowlist channel. Swappable in tests only.
var metricOut io.Writer = os.Stderr

// metric is the one-line stderr record. Raw conf/tokens/cost are NOT fields —
// only confBucket (0.8/0.9/1.0) crosses the boundary.
type metric struct {
	Seq         int    `json:"seq"`
	PID         int    `json:"pid"`
	TS          string `json:"ts,omitempty"`
	DurMS       int64  `json:"dur_ms,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Route       string `json:"route,omitempty"`
	MissReason  string `json:"miss_reason,omitempty"`
	Decision    string `json:"decision,omitempty"`
	Model       string `json:"model,omitempty"`
	KeyHash     string `json:"key_hash,omitempty"`
	ListenAddr  string `json:"listen_addr,omitempty"`
	Reason      string `json:"reason_code,omitempty"`
	Covered     bool   `json:"covered,omitempty"`
	Abstained   bool   `json:"abstained,omitempty"`
	Passthrough bool   `json:"passthrough,omitempty"`
	ConfBucket  string `json:"conf_bucket,omitempty"`
}

// confBucket coarsens a confidence to the allowlisted bucket. <0.8 is invalid
// (no verdict) and yields the empty bucket — never "0.8".
func confBucket(c float64) string {
	switch {
	case c >= 1:
		return "1.0"
	case c >= 0.9:
		return "0.9"
	case c >= 0.8:
		return "0.8"
	default:
		return ""
	}
}

// emitMetric writes exactly one JSON line; marshal failure emits nothing.
func emitMetric(m metric) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	fmt.Fprintln(metricOut, string(b))
}
