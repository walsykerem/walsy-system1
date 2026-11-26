package app

import "testing"

func TestAttemptGuardRevertsAtThreshold(t *testing.T) {
	g := newAttemptGuard()
	sig := "test:0123456789abcdef"
	for i := 1; i < attemptRevertAt; i++ {
		revert, steer := g.fail(sig)
		if revert || steer != "" {
			t.Fatalf("attempt %d reverted early: %v %q", i, revert, steer)
		}
		if got := g.attempts(sig); got != i {
			t.Fatalf("attempt %d counted %d", i, got)
		}
	}
	revert, steer := g.fail(sig)
	if !revert {
		t.Fatalf("attempt %d did not revert", attemptRevertAt)
	}
	if steer == "" {
		t.Fatal("revert without steer")
	}
	if got := g.attempts(sig); got != 0 {
		t.Fatalf("counter not reset after revert: %d", got)
	}
}

func TestAttemptGuardSeparatesSignatures(t *testing.T) {
	g := newAttemptGuard()
	for i := 0; i < attemptRevertAt-1; i++ {
		if revert, _ := g.fail("compile:aaa"); revert {
			t.Fatalf("signature aaa reverted on attempt %d", i+1)
		}
		if revert, _ := g.fail("compile:bbb"); revert {
			t.Fatalf("signature bbb reverted on attempt %d", i+1)
		}
	}
}

func TestAttemptGuardOKResets(t *testing.T) {
	g := newAttemptGuard()
	g.fail("test:aaa")
	g.fail("test:aaa")
	g.ok("test:aaa")
	if got := g.attempts("test:aaa"); got != 0 {
		t.Fatalf("ok left %d attempts", got)
	}
	if revert, _ := g.fail("test:aaa"); revert {
		t.Fatal("reverted one attempt after a pass")
	}
}

func TestAttemptGuardEmptySignature(t *testing.T) {
	g := newAttemptGuard()
	for i := 0; i < attemptRevertAt+2; i++ {
		if revert, steer := g.fail("  "); revert || steer != "" {
			t.Fatal("empty signature produced a verdict")
		}
	}
	if len(g.counts) != 0 {
		t.Fatalf("empty signature stored a counter: %v", g.counts)
	}
}
