package app

import (
	"testing"
)

// Permanent global simulation for the fix5 stable release. One run crosses the
// loopback System One decision boundary, then every fix5 guarantee in release
// order: trust-field scrubbing, state-root isolation, typed tool errors,
// bounded re-ask with cache, reader origin gate, receipt durability, retry
// parsing and version invalidation/cleanup. Each stage is a shipped focused
// test; running them as one flow is what makes the combined branch testable as
// a single stable release candidate.
func TestFix5GlobalSimulation(t *testing.T) {
	t.Run("00_loopback_so_decision", func(t *testing.T) {
		h := newEternalHarness(t)
		sc := eternalScenario{
			Name:    "fix5_global",
			Intent:  "stable fix5 release decision over the loopback oracle",
			Reply:   "yes",
			Expect:  "yes",
			Tool:    "systemone",
			Audit:   `{"todo":["stable fix5"],"done":["stable fix5"]}`,
			AuditOK: true,
		}
		if _, err := h.runScenario(t, sc); err != nil {
			t.Fatal(err)
		}
	})

	stages := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"01_trust_wire_scrubbed", TestSimFix502TrustFieldsReachSOWireScrubbed},
		{"04_state_outside_cache_root", TestFix504StateNeverInCacheRoot},
		{"05_typed_tool_error", TestToolsDecideInapplicableQuestionIsTyped},
		{"06_low_then_high_reask", TestSimFix506LowThenHighPromotesSameLabel},
		{"07_reader_origin_gate", TestFix5_07_SOStateReadersOriginGate},
		{"09_receipt_dir_fsync_creation_only", TestFix509ReceiptDirFsyncOnCreationOnly},
		{"10_retry_after_http_date", TestFix5_10RetryAfterHTTPDate},
		{"115_version_cleanup_upgrade", TestUpgradePackageVersionCleanup},
		{"117_version_invalidates_binary", TestBuildVersionInvalidatesInstalledBinary},
	}
	for _, s := range stages {
		t.Run(s.name, s.fn)
	}
}
