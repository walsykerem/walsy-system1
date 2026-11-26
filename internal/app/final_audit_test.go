package app

import (
	"strings"
	"testing"
)

func TestFinalAudit(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		ok     bool
		missed string // substring expected in Missing[0], if any
	}{
		{"all covered", `{"todo":["write tests"],"done":["write tests"]}`, true, ""},
		{"uncovered todo", `{"todo":["write tests"],"done":[]}`, false, "write tests"},
		{"partial todo", `{"todo":["write tests","wire route"],"done":["write tests"]}`, false, "wire route"},
		{"unverified risk", `{"risk":["api timeout"],"done":[]}`, false, "risk: api timeout"},
		{"verified risk", `{"risk":["api timeout"],"done":["verified api timeout under load"]}`, true, ""},
		{"denied risk does not cover", `{"risk":["api timeout"],"done":["did not verify api timeout"]}`, false, "risk: api timeout"},
		{"skipped todo does not cover", `{"todo":["write tests"],"done":["skipped write tests"]}`, false, "write tests"},
		{"untested todo does not cover", `{"todo":["wire route"],"done":["wire route untested"]}`, false, "wire route"},
		{"risk todo entry", `{"todo":["risk: disk full"],"done":[]}`, false, "risk: disk full"},
		{"restated risk is not coverage", `{"todo":["risk: disk full"],"done":["risk: disk full"]}`, false, "risk: disk full"},
		{"unverified risk alias", `{"unverified_risks":["key leak"],"done":[]}`, false, "risk: key leak"},
		{"no claims", `{"done":["ship it"]}`, true, ""},
		{"key folding", `{"todo":["Write  Tests"],"done":["write tests"]}`, true, ""},
		{"risk restated without evidence", `{"risk":["api timeout"],"done":["api timeout"]}`, false, "risk: api timeout"},
		{"risk still open", `{"risk":["api timeout"],"done":["verified api timeout still open"]}`, false, "risk: api timeout"},
		{"risk unresolved", `{"risk":["key leak"],"done":["confirmed key leak, unresolved"]}`, false, "risk: key leak"},
		{"risk no conclusion", `{"risk":["key leak"],"done":["confirmed key leak, no conclusion"]}`, false, "risk: key leak"},
		{"risk with evidence verb", `{"risk":["api timeout"],"done":["reproduced and confirmed api timeout"]}`, true, ""},
		{"unreadable fails closed", `not json`, false, "state unreadable"},
		{"empty fails closed", ``, false, "state unreadable"},
		{"wrong-typed state fails closed", `{"risk":"api timeout"}`, false, "state unreadable"},
	}
	for _, c := range cases {
		got := finalAudit(c.state)
		if got.OK != c.ok {
			t.Errorf("%s: ok=%v want %v (missing %v)", c.name, got.OK, c.ok, got.Missing)
		}
		if c.ok && (len(got.Missing) != 0 || got.Steer != "") {
			t.Errorf("%s: clean audit carries %v %q", c.name, got.Missing, got.Steer)
		}
		if !c.ok {
			if len(got.Missing) == 0 || got.Steer == "" {
				t.Errorf("%s: refusal without missing/steer", c.name)
			}
			found := false
			for _, m := range got.Missing {
				if strings.Contains(m, c.missed) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: missing list %v does not name %q", c.name, got.Missing, c.missed)
			}
		}
	}
}

func TestFinalAuditSteerNamesFirstThree(t *testing.T) {
	state := `{"todo":["a","b","c","d"],"done":[]}`
	got := finalAudit(state)
	if got.OK {
		t.Fatal("four open items audited clean")
	}
	if !strings.Contains(got.Steer, "+1 more") {
		t.Fatalf("steer does not count the remainder: %q", got.Steer)
	}
	if strings.Contains(got.Steer, "; d,") || strings.HasSuffix(got.Steer, "; d") {
		t.Fatalf("steer lists more than three: %q", got.Steer)
	}
}
