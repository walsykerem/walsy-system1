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
