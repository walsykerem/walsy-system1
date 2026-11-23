package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime/debug"
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

var healthBuildSHA = buildInfoValue("vcs.revision")

// healthFingerprint is the reproducible provenance surface. It never carries
// a key, raw prompt text, raw confidence or full endpoint credentials.
func healthFingerprint() map[string]any {
	items := sha256.Sum256(decisionItemsJSON)
	h := sha256.New()
	_, _ = h.Write(decisionItemsJSON)
	for _, p := range loadPrompts() {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(p.Name), p.Name, len(p.Text), p.Text)
	}
	_, _ = h.Write([]byte(healthBuildSHA))
	host := ""
	if u, err := url.Parse(soEndpoint); err == nil {
		host = u.Host
	}
	return map[string]any{
		"ok":               true,
		"build_sha":        healthBuildSHA,
		"vcs_modified":     buildInfoValue("vcs.modified") == "true",
		"items_sha256":     hex.EncodeToString(items[:]),
		"receipt_sha256":   hex.EncodeToString(h.Sum(nil)),
		"so_mode":          soMode(),
		"so_endpoint_host": host,
	}
}
