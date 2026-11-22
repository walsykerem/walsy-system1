package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	// passResult("x", ...) is route's refusal emit point.
	passResultLiteral = regexp.MustCompile(`passResult\("([a-z0-9-]+)"\s*[,)]`)
	// reason := "x" / reason = "x" feeds passResult indirectly.
	reasonVar = regexp.MustCompile(`reason\s*[:=]+\s*"([a-z0-9-]+)"`)
	// Reason: "x" is routeResult's admit/deny emit point.
	reasonField = regexp.MustCompile(`Reason:\s*"([a-z0-9-]+)"`)
	// classify maps SO failures to these codes.
	classifyReturn = regexp.MustCompile(`return "(transient|permanent|unknown|ok)"`)
	// guardReasonCode maps write-guard refusals to these codes.
	guardReturn = regexp.MustCompile(`return "(unread-write|unread-path|repeat|guard)"`)
	// d.Choice comparisons give the so-<choice> family its labels.
	choiceCompare = regexp.MustCompile(`d\.Choice == "([a-z]+)"`)
	// diffSteer's cases are the diff-* reason codes written by the write gate.
	diffSteerCode = regexp.MustCompile(`case "(diff-[a-z-]+)"`)
)

// derivedReasons reads the emitted reason vocabulary out of the source, so a
// new reason code without steer text fails TestSteerTextCoversReasons.
func derivedReasons(t *testing.T) []string {
	t.Helper()
	found := map[string]bool{}
	read := func(file string) string {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		return string(b)
	}
	scan := func(file string, pats ...*regexp.Regexp) {
		src := read(file)
		for _, re := range pats {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				found[m[1]] = true
			}
		}
	}
	scan("route.go", passResultLiteral, reasonVar, reasonField)
	scan("decision_bridge.go", passResultLiteral)
	scan("guard.go", classifyReturn)
	scan("write_guard.go", guardReturn)
	scan("diff_guard.go", diffSteerCode)
	scan("cache.go", reasonField)
	// diff codes never pass through the cache's passResult("cache-"+...).

	route := read("route.go")
	bridge := read("decision_bridge.go")
	// so-<choice>: every choice route refuses, minus the admitted "yes".
	if strings.Contains(route, `"so-"+`) {
		for _, m := range choiceCompare.FindAllStringSubmatch(route+read("cache.go"), -1) {
			if m[1] != "yes" {
				found["so-"+m[1]] = true
			}
		}
	}
	// cache-<reason>: cached refusals reuse the original code. diff-* refusals
	// return before the cache, so they never take this path.
	if strings.Contains(route, `"cache-"+`) || strings.Contains(bridge, `"cache-"+`) {
		for r := range found {
			if !strings.HasPrefix(r, "diff-") {
				found["cache-"+r] = true
			}
		}
	}
	// The three write-gate codes must all be present; an empty regex would
	// otherwise pass the generic steering check vacuously.
	for _, code := range []string{"diff-secret", "diff-wipe", "diff-destructive"} {
		if !found[code] {
			t.Errorf("reason %q missing from source scan", code)
		}
	}
	out := make([]string, 0, len(found))
	for r := range found {
		out = append(out, r)
	}
	return out
}

func TestSteerTextCoversReasons(t *testing.T) {
	reasons := derivedReasons(t)
	if len(reasons) < 10 {
		t.Fatalf("derived only %d reasons, source scan broke: %v", len(reasons), reasons)
	}
	for _, reason := range reasons {
		got := SteerText(reason, "")
		if strings.HasPrefix(reason, "diff-") {
			got = diffSteer(reason, "test")
		}
		if got == "" {
			t.Errorf("reason %q has no steer text", reason)
			continue
		}
		lower := strings.ToLower(got)
		for _, banned := range []string{"retry", "try again"} {
			if strings.Contains(lower, banned) {
				t.Errorf("reason %q steer invites a blind repeat (%q): %q", reason, banned, got)
			}
		}
	}
}

func TestSteerTextUsesLabel(t *testing.T) {
	if got := SteerText("registry-nonproceed", "chat"); !strings.Contains(got, `"chat"`) {
		t.Fatalf("registry-nonproceed steer dropped the label: %q", got)
	}
	if got := SteerText("so-low-conf", "maybe"); !strings.Contains(got, `"maybe"`) {
		t.Fatalf("so-low-conf steer dropped the label: %q", got)
	}
}

func TestSteerTextUnknown(t *testing.T) {
	for _, reason := range []string{"", "bogus-reason", "cache-bogus-reason"} {
		if got := SteerText(reason, ""); got != "" {
			t.Fatalf("SteerText(%q) = %q, want empty", reason, got)
		}
	}
}
