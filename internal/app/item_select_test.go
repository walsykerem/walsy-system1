package app

import "testing"

// The inference table is deterministic: same shape, same item, no model call.
func TestInferItem(t *testing.T) {
	cases := []struct {
		name, method, tool, phase, state, want string
	}{
		{"write to a named file", "tools/call", "write_file", "", "overwrite docs/plan.md", "write.unread"},
		{"write named in state", "tools/call", "edit", "", "edit src/route.go", "write.unread"},
		{"delete a path", "tools/call", "delete", "", "delete the old cache dir", "write.unread"},
		{"missing required args", "tools/call", "search", "", "missing required argument query", "tools.args"},
		{"invalid params", "tools/call", "db", "", "invalid params: -32602", "tools.args"},
		{"empty tool result", "", "search", "post-tool", "empty result: no output", "tools.result_sufficient"},
		{"empty error", "", "search", "post-tool", `{"error": null} empty error`, "tools.result_state"},
		{"build failure", "", "", "", "build failed: undefined: foo", "truth.failure_bucket"},
		{"final claim with skipped work", "", "", "", "task done, but the test run was skipped the check", "antishirk"},
		{"bare done claim", "", "", "", "done, ready to ship", "truth.done_claim"},
		{"ambiguous text stays generic", "", "", "", "classify this request", ""},
		{"json args claiming done stay generic", "", "bash", "",
			`{"arguments":{"command":"git commit -m done"},"name":"bash"}`, ""},
		{"empty frame stays generic", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferItem(tc.method, tc.tool, tc.phase, tc.state); got != tc.want {
				t.Fatalf("inferItem(%q,%q,%q,%q)=%q want %q",
					tc.method, tc.tool, tc.phase, tc.state, got, tc.want)
			}
		})
	}
}
