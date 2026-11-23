package main

import (
	"os"
	"strings"
	"testing"
)

// excerpt inlines the cited bytes, so the resolver can prove a cited line
// exists rather than trusting plausible prose.
func TestExcerptInlinesCitedArtifact(t *testing.T) {
	t.Chdir("../..")
	// Find the cited line by content: production edits move `decide: ` between
	// line numbers; the invariant is that those exact bytes get inlined.
	b, err := os.ReadFile("decision_bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"decide: "`) {
			line = i + 1
			break
		}
	}
	if line == 0 {
		t.Fatal(`decision_bridge.go: no line contains "decide: "`)
	}
	ex, err := excerpt(citation{"decision_bridge.go", line, line})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex, "decide: ") {
		t.Fatalf("cited line not inlined:\n%s", ex)
	}
}

// Every shipped claim must resolve the artifacts it cites; a drift (moved file
// or line) fails here instead of silently marking the claim OPEN at run time.
func TestAllClaimsResolve(t *testing.T) {
	t.Chdir("../..")
	for _, c := range claims {
		if err := resolve(c); err != nil {
			t.Errorf("%s: %v", c.id, err)
		}
	}
}

// A missing citation, path or out-of-range line must fail closed: resolve
// returns an error so the claim is counted OPEN instead of scored.
func TestResolveFailsClosed(t *testing.T) {
	t.Chdir("../..")
	if err := resolve(claim{"K", "no path named here"}); err == nil {
		t.Fatal("missing citation did not fail closed")
	}
	if err := resolve(claim{"K", "see no_such_file.go:1"}); err == nil {
		t.Fatal("missing cited path did not fail closed")
	}
	if err := resolve(claim{"K", "see decision_bridge.go:99999"}); err == nil {
		t.Fatal("out-of-range cited line did not fail closed")
	}
}
