package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

// diffVerdict is the local precheck over a proposed write. Refuse is final for
// this call: the caller turns Code into a reason-code and Steer into the next
// action, never a model call. A clean diff is Refuse=false with empty fields.
type diffVerdict struct {
	Refuse bool
	Code   string
	Steer  string
}

// diffDeleteLines is the pure-deletion size that gets a second look. A diff
// that only removes lines is the shape of an accidental wipe, not a fix.
const diffDeleteLines = 20

var diffSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN[ A-Z]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT bearer token
	regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{12,}`),                   // prefixed client secret / key
}

// diffSecretAssign matches a credential key assigned a quoted or bare literal.
var diffSecretAssign = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_-]?key|secret[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|private[_-]?key|token|aws_secret_access_key|aws_access_key_id)\b\s*[:=]\s*("([^"]*)"|'([^']*)'|([^\s,;#]{6,}))`)

// diffSecurityCheck is the write gate's security precheck: it scans only the
// lines the write ADDS, so a file that already carries a secret does not block
// every later edit. It also refuses an emptying write and a pure deletion.
// Pure: no I/O, no network, no model, no globals written.
// shortcut: line-multiset diff, not an LCS; a reordered line counts as added,
// which is the safe direction. Upgrade when reorder-only edits start refusing.
func diffSecurityCheck(path, oldContent, newContent string) diffVerdict {
	added, removed := diffLines(oldContent, newContent)
	for _, line := range added {
		if kind := diffSecretKind(line); kind != "" {
			return diffVerdict{Refuse: true, Code: "diff-secret", Steer: diffSteer("diff-secret", kind)}
		}
	}
	if strings.TrimSpace(newContent) == "" && strings.TrimSpace(oldContent) != "" {
		return diffVerdict{Refuse: true, Code: "diff-wipe", Steer: diffSteer("diff-wipe", path)}
	}
	if len(added) == 0 && len(removed) >= diffDeleteLines {
		return diffVerdict{Refuse: true, Code: "diff-destructive", Steer: diffSteer("diff-destructive", path)}
	}
	return diffVerdict{}
}

// oldContentOnDisk lets the diff gate judge a wipe or a pure deletion against
// what the file really holds. A read error or an oversized file falls back to
// empty, which only loses the wipe/destructive arms, never the secret arm.
// shortcut: 4 MiB cap, raise when larger files need wipe detection.
func oldContentOnDisk(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4<<20 {
		return ""
	}
	return string(b)
}

// argsSecretCheck covers a call that carries no file body to diff: a shell
// redirect, or a tool whose body key is not one of the five known ones. It
// scans the raw arguments, which is the secure direction: a credential shape
// anywhere in a tool call is refused rather than assumed harmless.
// shortcut: shape scan over the argument text, not a parse of every schema;
// upgrade when a tool legitimately needs to pass a key to a secret store.
func argsSecretCheck(args json.RawMessage) diffVerdict {
	if kind := diffSecretKind(string(args)); kind != "" {
		return diffVerdict{Refuse: true, Code: "diff-secret", Steer: diffSteer("diff-secret", kind)}
	}
	return diffVerdict{}
}

// diffSecretKind names what refused, without echoing the secret itself.
func diffSecretKind(line string) string {
	for _, re := range diffSecretPatterns {
		if re.MatchString(line) {
			return "credential-shaped literal"
		}
	}
	m := diffSecretAssign.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	val := m[3]
	if val == "" {
		val = m[4]
	}
	if val == "" {
		val = m[5]
	}
	return literalSecretKind(m[1], val)
}

// literalSecretKind reports a literal credential assignment. Env refs and
// placeholders are the correct pattern and pass.
func literalSecretKind(key, val string) string {
	v := strings.TrimSpace(val)
	if len(v) < 6 {
		return ""
	}
	lower := strings.ToLower(v)
	for _, skip := range []string{"$", "os.", "os.getenv", "env.", "process.env", "$(", "{{", "<", "your", "xxx", "changeme", "change-me", "placeholder", "redacted", "example", "dummy", "todo", "****"} {
		if strings.HasPrefix(lower, skip) || strings.Contains(lower, skip) {
			return ""
		}
	}
	// A literal credential value of 6+ chars is refused even when it is all
	// lowercase with no digits (the 15-char lowercase case). Env refs and
	// placeholders were already skipped above.
	return key + " assigned a literal value"
}

// diffLines returns the added and removed lines as multisets: duplicate lines
// are counted, so moving a duplicated line does not look like an addition.
func diffLines(oldContent, newContent string) (added, removed []string) {
	have := map[string]int{}
	for _, l := range strings.Split(oldContent, "\n") {
		have[strings.TrimSuffix(l, "\r")]++
	}
	for _, l := range strings.Split(newContent, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if have[l] > 0 {
			have[l]--
			continue
		}
		added = append(added, l)
	}
	kept := map[string]int{}
	for _, l := range strings.Split(newContent, "\n") {
		kept[strings.TrimSuffix(l, "\r")]++
	}
	for _, l := range strings.Split(oldContent, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if kept[l] > 0 {
			kept[l]--
			continue
		}
		removed = append(removed, l)
	}
	return added, removed
}

// diffSteer is the one concrete next action per refusal code.
func diffSteer(code, detail string) string {
	switch code {
	case "diff-secret":
		return "keep the secret in the environment or a key store and write a reference instead; the " + detail + " stays in local history."
	case "diff-wipe":
		return "this write empties " + detail + ": delete the file deliberately, or write the content it should hold."
	case "diff-destructive":
		return "this diff only removes lines from " + detail + ": confirm the deletion is intended, or split it into a smaller reviewed change."
	}
	return ""
}
