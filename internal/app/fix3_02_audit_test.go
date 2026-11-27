package app

import "testing"

// A risk whose own name contains a deny word must still be coverable by an
// evidence sentence that names it. "pending migration" can only ever be
// mentioned together with the word "pending", so the deny scan must judge the
// sentence without the risk name it is checking.
func TestFix302RiskKeyDenyWordSelfBlock(t *testing.T) {
	cases := []string{
		`{"risk":["pending migration"],"done":["verified pending migration ordering under load"]}`,
		`{"todo":["risk: open connections"],"done":["tested open connections stay bounded at 10k"]}`,
		`{"unverified_risks":["unknown tool handling"],"done":["reproduced unknown tool handling with loopback stub"]}`,
	}
	for _, state := range cases {
		if v := finalAudit(state); !v.OK {
			t.Errorf("finalAudit(%s) = OK=false Missing=%v, want OK (evidence sentence names the risk)", state, v.Missing)
		}
	}
	// A real denial that also names a deny-word risk must still be refused.
	denied := `{"risk":["pending migration"],"done":["not verified pending migration"]}`
	if v := finalAudit(denied); v.OK {
		t.Errorf("finalAudit(%s) = OK=true, want refusal: the sentence denies the work", denied)
	}
}
