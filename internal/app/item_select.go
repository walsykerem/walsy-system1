package app

import "strings"

// Item inference: every relayed frame used to get the generic yes/no/other
// question, so the registry only fired when a caller passed an explicit item.
// inferItem names an item from the frame's own shape instead: a fixed rule
// table, no model call, no free-text guessing. No rule and weak evidence both
// return "", which keeps the generic path byte-for-byte unchanged.

type itemRule struct {
	item  string
	prose bool // claims only: never a raw JSON argument/result envelope
	match func(method, tool, phase, text string) bool
}

// itemRules is ordered most-specific-first; the first match wins. text is the
// lowercased tool + phase + state, so every match is a fixed substring check.
var itemRules = []itemRule{
	// A tools/call whose arguments do not satisfy the tool's own schema.
	{item: "tools.args", match: func(method, tool, _, t string) bool {
		return (method == "tools/call" || tool != "") && containsAny(t,
			"missing required", "missing argument", "missing parameter",
			"required argument", "invalid argument", "invalid params",
			"malformed", "unexpected end of json", "-32602")
	}},
	// A write verb aimed at a named file/path is a write before the read.
	{item: "write.unread", match: func(_, tool, _, t string) bool {
		// Whole-segment match: a read-only name like "readwrite" must not count.
		write := isWriteTool(tool)
		if !write {
			// Prose fallback over phase+state only: the text starts with the tool
			// name (already judged above), so stripping it stops "readwrite"
			// satisfying "write " through substring matching.
			prose := strings.TrimPrefix(t, tool+" ")
			write = containsAny(prose, "write ", "writing ", "overwrite", "edit ", "editing ",
				"create ", "creating ", "delete ", "deleting ", "rename ", "renaming ", "patch ")
		}
		return write && containsAny(t, "file", "path", "dir", "/", ".go", ".md",
			".json", ".py", ".ts", ".js", ".yaml", ".yml", ".sh")
	}},
	// A tool result that came back empty or cut short.
	{item: "tools.result_sufficient", match: func(_, _, phase, t string) bool {
		return isToolResult(phase) && containsAny(t, "empty result", "no result",
			"no output", "blank output", "truncated", "partial result", "insufficient")
	}},
	// A tool result that errored without saying anything usable.
	{item: "tools.result_state", match: func(_, _, phase, t string) bool {
		return isToolResult(phase) && containsAny(t,
			`"error":null`, `"error": null`, `"error":""`, `"error": ""`,
			"empty error", "error is empty")
	}},
	// Build/test output that failed: bucket it before retrying or claiming.
	{item: "truth.failure_bucket", prose: true, match: func(_, _, _, t string) bool {
		return containsAny(t, "build failed", "build error", "tests failed",
			"test failed", "test failures", "failing test", "compile error",
			"cannot find package", "panic:", "exit status")
	}},
	// A "finished" claim that also names skipped or unverified work.
	{item: "antishirk", prose: true, match: func(_, _, _, t string) bool {
		return containsAny(t, "not verified", "unverified", "did not run",
			"didn't run", "skipped the", "skipped ", "skipping ", "deferred",
			"left for later", "shortcut", "unfinished", "uncovered")
	}},
	// A bare "done"/final claim with no verification evidence in the text.
	{item: "truth.done_claim", prose: true, match: func(_, _, _, t string) bool {
		return containsAny(t, "done", "finished", "complete", "ready to ship",
			"all tests pass", "works now", "shipped")
	}},
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// isToolResult: MCP results arrive as response frames in a post-tool phase.
func isToolResult(phase string) bool {
	return strings.Contains(phase, "post") || strings.Contains(phase, "result") ||
		strings.Contains(phase, "tool")
}

// inferItem is the pure selector: (jsonrpc method, tool name, phase, state
// text) -> registry item id, or "" when the shape is unknown or ambiguous.
func inferItem(method, tool, phase, state string) string {
	method = strings.ToLower(strings.TrimSpace(method))
	tool = strings.ToLower(strings.TrimSpace(tool))
	phase = strings.ToLower(phase)
	text := tool + " " + phase + " " + strings.ToLower(state)
	// A raw argument envelope is data: a command that merely says "done" must
	// not steer the item. Result-item rules still read JSON shapes.
	dataEnvelope := strings.HasPrefix(strings.TrimSpace(state), "{")
	for _, r := range itemRules {
		if r.prose && dataEnvelope {
			continue
		}
		if !r.match(method, tool, phase, text) {
			continue
		}
		if _, known := decisionItemFor(r.item); !known {
			return "" // a stale rule must never carry an id the registry lost
		}
		return r.item
	}
	return ""
}
