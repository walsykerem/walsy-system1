package app

import "testing"

// TestFinalAudit pins the risk-coverage rules in finalAudit: a risk is covered
// only by a done entry that names it AND states evidence, and any denying word
// voids that entry. An unreadable or wrong-typed state fails closed.
func TestFinalAudit(t *testing.T) {
	cases := []struct {
		name  string
		state string
		ok    bool
	}{
		{"restated risk is not coverage", `{"risk":["api timeout"],"done":["api timeout"]}`, false},
		{"risk restated without evidence", `{"risk":["api timeout"],"done":["api timeout handled"]}`, false},
		{"verified risk", `{"risk":["api timeout"],"done":["verified api timeout"]}`, true},
		{"risk with evidence verb", `{"risk":["api timeout"],"done":["reproduced and confirmed api timeout"]}`, true},
		{"risk still open", `{"risk":["api timeout"],"done":["verified api timeout still open"]}`, false},
		{"risk unresolved", `{"risk":["key leak"],"done":["confirmed key leak, unresolved"]}`, false},
		{"risk no conclusion", `{"risk":["api timeout"],"done":["checked api timeout, no conclusion"]}`, false},
		{"unreadable fails closed", `not json`, false},
		{"empty fails closed", ``, false},
		{"wrong-typed state fails closed", `{"risk":"api timeout"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := finalAudit(tc.state)
			if v.OK != tc.ok {
				t.Fatalf("finalAudit(%q).OK = %v, want %v (missing=%v)", tc.state, v.OK, tc.ok, v.Missing)
			}
			if !v.OK && v.Steer == "" {
				t.Fatalf("finalAudit(%q): failing verdict has no steer", tc.state)
			}
		})
	}
}
