package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime/debug"
	"sync"
)

// buildInfoValue reads one stamped build setting. It returns empty when the
// binary was built without VCS stamping (for example, go test).
func buildInfoValue(key string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// healthSink is the consumer-side observability surface: coarse -32001 denial
// counts and the last degraded/no-answer reason. No prompts, labels or keys.
var healthSink = struct {
	mu           sync.Mutex
	denials      map[string]int
	decisionNote string
	degraded     bool
}{denials: map[string]int{}}

func recordDenial(reason string) {
	healthSink.mu.Lock()
	defer healthSink.mu.Unlock()
	if reason == "" {
		reason = "denied"
	}
	healthSink.denials[reason]++
}

func recordDecisionReason(reason string) {
	if reason != "so-degraded" && reason != "so-noul" {
		return
	}
	healthSink.mu.Lock()
	defer healthSink.mu.Unlock()
	healthSink.decisionNote = reason
	healthSink.degraded = true
}

var healthBuildSHA = buildInfoValue("vcs.revision")

// healthFingerprint is the reproducible provenance surface. It never carries
// a key, raw prompt text, raw confidence or full endpoint credentials.
func healthFingerprint() map[string]any {
	return healthFingerprintWithEndpoint(soEndpoint)
}

// healthFingerprintWithEndpoint lets async receipt writers use an endpoint
// captured at bridge construction instead of racing cleanup of the global.
func healthFingerprintWithEndpoint(endpoint string) map[string]any {
	items := sha256.Sum256(decisionItemsJSON)
	h := sha256.New()
	_, _ = h.Write(decisionItemsJSON)
	for _, p := range loadPrompts() {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(p.Name), p.Name, len(p.Text), p.Text)
	}
	_, _ = h.Write([]byte(healthBuildSHA))
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Host
	}
	admit := admitStats.snapshot()
	healthSink.mu.Lock()
	denials := make(map[string]int, len(healthSink.denials))
	totalDenials := 0
	for reason, n := range healthSink.denials {
		denials[reason] = n
		totalDenials += n
	}
	decisionNote, degraded := healthSink.decisionNote, healthSink.degraded
	healthSink.mu.Unlock()
	healthState := "ok"
	if degraded {
		healthState = "degraded"
	} else if totalDenials > 0 {
		healthState = "denying"
	}

	return map[string]any{
		"ok":           true,
		"build_sha":    healthBuildSHA,
		"vcs_modified": buildInfoValue("vcs.modified") == "true",
		"items_sha256": hex.EncodeToString(items[:]),
		// shortcut: digest is the detached-signature surface until the shipped
		// .sig artifact exists; swap to a verified signature when verify.sh lands.
		"items_signature":  hex.EncodeToString(items[:]),
		"receipt_sha256":   hex.EncodeToString(h.Sum(nil)),
		"so_mode":          soMode(),
		"so_endpoint_host": host,
		"decisions":        admit.N,
		"fail_open":        admit.Blocked,
		"admit_counts": map[string]any{
			"n": admit.N, "admits": admit.Admits, "blocked": admit.Blocked, "reasons": admit.Reasons,
		},
		"health_state":         healthState,
		"last_decision_reason": decisionNote,
		"denials": map[string]any{
			"total": totalDenials, "reasons": denials,
		},
		"so_health": map[string]any{
			"breaker_open": routerBreakerOpen("systemone"),
			"state":        healthState,
		},
		"degraded": healthState == "degraded",
		"release_aliases": []string{
			"walsy-system1", "systemone", "system one", "Smart Router (System One)",
		},
		"release": map[string]any{
			"version": "v0.5.109",
			"pr": map[string]any{
				"number":       4975,
				"merged_at":    nil,
				"code_present": true,
			},
		},
	}
}
