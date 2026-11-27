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

// Restating a risk name is not evidence, even when the name contains an
// evidence substring ("verified" inside "unverified").
func TestFix302RiskNameAloneIsNotProof(t *testing.T) {
	cases := []string{
		`{"risk":["unverified tool"],"done":["unverified tool"]}`,
		`{"risk":["untested feature"],"done":["untested feature"]}`,
	}
	for _, state := range cases {
		if v := finalAudit(state); v.OK {
			t.Errorf("finalAudit(%s) = OK=true, want refusal: name alone is not proof", state)
		}
	}
	// Explicit evidence outside the risk name stays coverable.
	ok := `{"risk":["unverified tool"],"done":["verified unverified tool"]}`
	if v := finalAudit(ok); !v.OK {
		t.Errorf("finalAudit(%s) = OK=false Missing=%v, want OK", ok, v.Missing)
	}
}

// Denials outside the single risk mention must survive, including ones that
// share letters with the risk key or repeat the key.
func TestFix302DenialOutsideRiskMentionIsKept(t *testing.T) {
	cases := []string{
		`{"risk":["test"],"done":["test verified, the suite was untested"]}`,
		`{"risk":["open"],"done":["verified open but still open"]}`,
	}
	for _, state := range cases {
		if v := finalAudit(state); v.OK {
			t.Errorf("finalAudit(%s) = OK=true, want refusal: denial outside the risk mention", state)
		}
	}
}
