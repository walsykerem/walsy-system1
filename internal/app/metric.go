package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// metricOut is the single stderr allowlist channel. Swappable in tests only.
var metricOut io.Writer = os.Stderr

// metric is the one-line stderr record. Raw conf/tokens/cost are NOT fields -
// only confBucket (0.8/0.9/1.0) crosses the boundary.
type metric struct {
	Seq          int    `json:"seq"`
	PID          int    `json:"pid"`
	Route        string `json:"route,omitempty"`
	Decision     string `json:"decision,omitempty"`
	UpstreamExit string `json:"upstream_exit,omitempty"` // "error" only; never a secret
	ListenAddr   string `json:"listen_addr,omitempty"`
	Reason       string `json:"reason_code,omitempty"`
	Covered      bool   `json:"covered,omitempty"`
	Abstained    bool   `json:"abstained,omitempty"`
	Passthrough  bool   `json:"passthrough,omitempty"`
	ConfBucket   string `json:"conf_bucket,omitempty"`
	Source       string `json:"source,omitempty"` // "preflight" on advisory shadow records only
	Hash         string `json:"hash,omitempty"`   // coarse digest of the allowlisted record
	Label        string `json:"label,omitempty"`  // coarse decision/reason label
	LatencyMS    int64  `json:"latency_ms,omitempty"`
}

// confBucket coarsens a confidence to the allowlisted bucket. <0.8 is invalid
// (no verdict) and yields the empty bucket - never "0.8".
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
// hash/label are derived from allowlisted fields only, so every record carries
// them without changing the production call sites.
func emitMetric(m metric) {
	if m.Hash == "" {
		if core, err := json.Marshal(m); err == nil {
			sum := sha256.Sum256(core)
			m.Hash = hex.EncodeToString(sum[:])[:12]
		}
	}
	if m.Label == "" {
		m.Label = m.Decision
		if m.Label == "" {
			m.Label = m.Reason
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	fmt.Fprintln(metricOut, string(b))
}
