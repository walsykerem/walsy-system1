package app

import (
	"strings"
	"testing"
)

// fix4/04: destructive work runs only after three independent SO decisions at
// >=0.8. Every action here is a fake callback; no file, branch or process is
// touched. No HTTP: the decision responder is injected.

type fix404Ask struct {
	answers   []soDecision
	states    []string
	questions []soQuestion
}

func (a *fix404Ask) ask(state string, q soQuestion) soDecision {
	a.states = append(a.states, state)
	a.questions = append(a.questions, q)
	if len(a.answers) == 0 {
		return soDecision{}
	}
	d := a.answers[0]
	a.answers = a.answers[1:]
	return d
}

func fix404Approve(conf float64) soDecision {
	return soDecision{Choice: "approve", Conf: conf, AnswerType: "choice"}
}

func fix404Deny(conf float64) soDecision {
	return soDecision{Choice: "deny", Conf: conf, AnswerType: "choice"}
}

func fix404Intent() destructiveIntent {
	return destructiveIntent{Action: "force push", Target: "origin/fix4", Reason: "recovery", Undo: "backup ref"}
}

func fix404Request(ask *fix404Ask, ran, rolledBack *int) destructiveRequest {
	return destructiveRequest{
		Intent:   fix404Intent(),
		Ask:      ask.ask,
		Run:      func() error { *ran++; return nil },
		Rollback: func() { *rolledBack++ },
	}
}

func TestFix404ThreeIndependentApprovalsRunAction(t *testing.T) {
	ask := &fix404Ask{answers: []soDecision{fix404Approve(0.81), fix404Approve(0.90), fix404Approve(0.97)}}
	ran, rolled := 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if !got.Approved || got.Reason != "" {
		t.Fatalf("three admits must run the action: %+v", got)
	}
	if ran != 1 || rolled != 0 || ask.askCalls() != 3 {
		t.Fatalf("ran=%d rollback=%d calls=%d, want 1/0/3", ran, rolled, ask.askCalls())
	}
	if len(got.Verdicts) != 3 {
		t.Fatalf("verdicts=%d, want 3", len(got.Verdicts))
	}
	seen := map[string]bool{}
	for i, v := range got.Verdicts {
		if v.Slot != i+1 || v.Label != "approve" || v.Confidence != ask.answersConf(i) {
			t.Fatalf("verdict %d = %+v", i, v)
		}
		if seen[v.Fingerprint] {
			t.Fatalf("verdict %d reuses an earlier answer: %+v", i, v)
		}
		seen[v.Fingerprint] = true
		if v.StateDigest == "" || v.Question == "" || v.Retried {
			t.Fatalf("verdict %d missing audit fields: %+v", i, v)
		}
	}
	// Intent reaches the model, not just a generic yes/no envelope.
	for i, state := range ask.states {
		for _, word := range []string{"force push", "origin/fix4", "not-started", "0.8"} {
			if !strings.Contains(state, word) {
				t.Fatalf("state %d lacks %q: %s", i, word, state)
			}
		}
	}
}

func (a *fix404Ask) askCalls() int { return len(a.states) }

// answersConf maps the verdict slot to the scripted confidence; answers are
// consumed, so keep a parallel record.
func (a *fix404Ask) answersConf(slot int) float64 {
	return []float64{0.81, 0.90, 0.97}[slot]
}

func TestFix404WeakVerdictRetriestighterStateAndQuestion(t *testing.T) {
	ask := &fix404Ask{answers: []soDecision{
		fix404Approve(0.41), fix404Approve(0.88),
		fix404Approve(0.92), fix404Approve(0.99),
	}}
	ran, rolled := 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if !got.Approved || ran != 1 || ask.askCalls() != 4 {
		t.Fatalf("weak first verdict must re-ask: approved=%v ran=%d calls=%d (%+v)", got.Approved, ran, ask.askCalls(), got)
	}
	if !got.Verdicts[0].Retried || got.Verdicts[0].Confidence != 0.88 {
		t.Fatalf("stored verdict must be the tightened retry: %+v", got.Verdicts[0])
	}
	if !strings.Contains(ask.states[1], "tightened") || ask.states[0] == ask.states[1] {
		t.Fatalf("retry state not tightened: %q -> %q", ask.states[0], ask.states[1])
	}
	if ask.questions[0].Instructions == ask.questions[1].Instructions {
		t.Fatal("retry reused the same question")
	}
	if rolled != 0 {
		t.Fatalf("successful approval rolled back %d times", rolled)
	}
}

func TestFix404StillWeakAfterRetryCancels(t *testing.T) {
	ask := &fix404Ask{answers: []soDecision{fix404Approve(0.5), fix404Approve(0.5)}}
	ran, rolled := 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if got.Approved || got.Reason != "approval-cancelled" || ran != 0 || rolled == 0 {
		t.Fatalf("weak after retry must cancel: %+v ran=%d rolled=%d", got, ran, rolled)
	}
	if ask.askCalls() != 2 || len(got.Verdicts) != 1 {
		t.Fatalf("calls=%d verdicts=%d, want 2/1", ask.askCalls(), len(got.Verdicts))
	}
}

func TestFix404DuplicateVerdictIsNotAnIndependentApproval(t *testing.T) {
	ask := &fix404Ask{answers: []soDecision{fix404Approve(0.9), fix404Approve(0.9)}}
	ran, rolled := 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if got.Approved || got.Reason != "duplicate-verdict" || ran != 0 || rolled == 0 {
		t.Fatalf("repeated answer must not count: %+v ran=%d rolled=%d", got, ran, rolled)
	}
	if len(got.Verdicts) != 2 {
		t.Fatalf("verdicts=%d, want 2 stored for audit", len(got.Verdicts))
	}
}

func TestFix404AnyDropCancelsWithoutRunning(t *testing.T) {
	ask := &fix404Ask{answers: []soDecision{
		fix404Approve(0.9), fix404Deny(0.99), fix404Deny(0.99),
	}}
	ran, rolled := 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if got.Approved || got.Reason != "approval-cancelled" || ran != 0 || rolled == 0 {
		t.Fatalf("a dropped gate must cancel: %+v ran=%d rolled=%d", got, ran, rolled)
	}
	if len(got.Verdicts) != 2 {
		t.Fatalf("verdicts=%d, want 2 stored for audit", len(got.Verdicts))
	}
}

func TestFix404ThresholdIsExactlyPointEight(t *testing.T) {
	// Identical answers are replays; vary ResponseID so the three 0.8
	// decisions are independent while still probing the exact threshold.
	atCut := func(id string) soDecision {
		d := fix404Approve(0.8)
		d.ResponseID = id
		return d
	}
	ask := &fix404Ask{answers: []soDecision{atCut("cut-1"), atCut("cut-2"), atCut("cut-3")}}
	ran, rolled := 0, 0
	if got := approveDestructive(fix404Request(ask, &ran, &rolled)); !got.Approved || ran != 1 {
		t.Fatalf("confidence == 0.8 must admit: %+v ran=%d", got, ran)
	}
	ask = &fix404Ask{answers: []soDecision{fix404Approve(0.799), fix404Approve(0.799)}}
	ran, rolled = 0, 0
	got := approveDestructive(fix404Request(ask, &ran, &rolled))
	if got.Approved || ran != 0 {
		t.Fatalf("confidence < 0.8 must cancel: %+v ran=%d", got, ran)
	}
}

func TestFix404MalformedRequestNeverRuns(t *testing.T) {
	ran, rolled := 0, 0
	got := approveDestructive(destructiveRequest{
		Intent:   fix404Intent(),
		Run:      func() error { ran++; return nil },
		Rollback: func() { rolled++ },
	})
	if got.Approved || got.Reason != "invalid-request" || ran != 0 {
		t.Fatalf("no decision responder must fail closed: %+v ran=%d", got, ran)
	}
}

// BenchmarkFix404Approval measures the in-memory envelope only: the real flow
// is dominated by three SO POSTs, so a Rust port of this logic gains nothing.
func BenchmarkFix404Approval(b *testing.B) {
	ask := &fix404Ask{}
	run := func() error { return nil }
	rollback := func() {}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ask.answers = []soDecision{fix404Approve(0.81), fix404Approve(0.90), fix404Approve(0.97)}
		ask.states = ask.states[:0]
		ask.questions = ask.questions[:0]
		_ = approveDestructive(destructiveRequest{Intent: fix404Intent(), Ask: ask.ask, Run: run, Rollback: rollback})
	}
}
