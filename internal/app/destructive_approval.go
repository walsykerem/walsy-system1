package app

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Destructive work (delete, reset, force push, deploy, spend, ...) runs only
// after three independent decision-model approvals. One weak or duplicate
// verdict cancels the whole operation before the action is invoked.
const (
	destructiveSlots         = 3
	destructiveConfidenceCut = 0.8
)

type destructiveIntent struct {
	Action string // exact operation, e.g. "force push"
	Target string // exact target, e.g. "origin/fix4"
	Reason string // why the loss is intended
	Undo   string // rollback path, or "none"
}

// destructiveVerdict is the audit record for one decision slot. Only digests
// and the typed label/confidence are kept: no raw state or key material.
type destructiveVerdict struct {
	Slot        int     `json:"slot"`
	Label       string  `json:"label"`
	Confidence  float64 `json:"confidence"`
	Question    string  `json:"question"`
	StateDigest string  `json:"state_digest"`
	Fingerprint string  `json:"fingerprint"`
	Retried     bool    `json:"retried"`
}

type destructiveApproval struct {
	Approved bool                 `json:"approved"`
	Reason   string               `json:"reason,omitempty"`
	Verdicts []destructiveVerdict `json:"verdicts"`
	Calls    int                  `json:"calls"`
}

// Ask and Run are callbacks so the flow is testable with a fake action: no
// filesystem, branch or network operation lives in this file. Ask is the real
// askSOQ in production; the tightened re-ask uses the same injected responder.
type destructiveRequest struct {
	Intent   destructiveIntent
	Ask      func(state string, q soQuestion) soDecision
	Run      func() error
	Rollback func()
}

func destructiveClean(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	if len(v) > 160 {
		v = v[:160]
	}
	if v == "" {
		return "unspecified"
	}
	return v
}

func destructiveState(it destructiveIntent, slot int, tightened bool) string {
	action, target := destructiveClean(it.Action), destructiveClean(it.Target)
	lines := []string{
		"destructive-operation-approval",
		"slot: " + strconv.Itoa(slot) + " of " + strconv.Itoa(destructiveSlots),
		"intent: " + action + " on " + target,
		"action: " + action,
		"target: " + target,
		"reason: " + destructiveClean(it.Reason),
		"reversible_by: " + destructiveClean(it.Undo),
		"operation_status: not-started",
		"threshold: confidence >= 0.8",
	}
	if tightened {
		lines = append(lines,
			"tightened: exact action, exact target, exact loss; no ambiguity and no restatement",
			"intent_restated: approve only "+action+" on "+target+"; the loss has not happened yet")
	}
	return strings.Join(lines, "\n")
}

func destructiveQuestion(slot int, tightened bool) soQuestion {
	instructions := "Judge the stated intent only: this destructive operation has not started. Approve only if this exact action on this exact target is intended and its loss is accepted."
	if tightened {
		instructions = "Tighter re-check of the same intent: name no alternative action or target. Approve only if the stated destructive operation on the stated target is unambiguously intended, not yet run, and its loss is accepted."
	}
	return soQuestion{Type: "choice",
		Instructions: instructions + " Independent decision " + strconv.Itoa(slot) + " of " + strconv.Itoa(destructiveSlots) + ".",
		Criteria: map[string]string{
			"approve": "the stated destructive operation is intended and may proceed",
			"deny":    "do not run this destructive operation",
		},
	}
}

func destructiveApproved(d soDecision) bool {
	return d.Choice == "approve" && !d.TimedOut && d.Reason == "" &&
		(d.AnswerType == "" || d.AnswerType == "choice") &&
		!math.IsNaN(d.Conf) && !math.IsInf(d.Conf, 0) && d.Conf >= destructiveConfidenceCut
}

// destructiveFingerprint hashes the answer alone, not the prompt: the same
// label/confidence returned twice is a replay, not a second independent vote.
func destructiveFingerprint(d soDecision) string {
	h := sha256.New()
	writeField(h, d.Choice)
	writeField(h, strconv.FormatFloat(d.Conf, 'g', -1, 64))
	writeField(h, d.AnswerType)
	writeField(h, d.ResponseID)
	keys := make([]string, 0, len(d.Probabilities))
	for k := range d.Probabilities {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, k)
		writeField(h, strconv.FormatFloat(d.Probabilities[k], 'g', -1, 64))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// approveDestructive posts one decision per slot. A weak/absent answer gets
// one re-ask with a tightened state and revised question; more ambiguity
// cancels. shortcut: one re-ask per slot only, then cancel instead of looping.
func approveDestructive(r destructiveRequest) destructiveApproval {
	out := destructiveApproval{Verdicts: make([]destructiveVerdict, 0, destructiveSlots)}
	if strings.TrimSpace(r.Intent.Action) == "" || strings.TrimSpace(r.Intent.Target) == "" ||
		r.Ask == nil || r.Run == nil {
		out.Reason = "invalid-request"
		return out
	}
	cancel := func(reason string) destructiveApproval {
		out.Approved = false
		out.Reason = reason
		if r.Rollback != nil {
			r.Rollback()
		}
		return out
	}
	seen := make(map[string]bool, destructiveSlots)
	for slot := 1; slot <= destructiveSlots; slot++ {
		state := destructiveState(r.Intent, slot, false)
		q := destructiveQuestion(slot, false)
		d := r.Ask(state, q)
		out.Calls++
		retried := false
		if !destructiveApproved(d) {
			// Weak/no verdict: tighten the state, revise the question and
			// restate the intent so the model is judging the same operation.
			state = destructiveState(r.Intent, slot, true)
			q = destructiveQuestion(slot, true)
			d = r.Ask(state, q)
			out.Calls++
			retried = true
		}
		fp := destructiveFingerprint(d)
		sum := sha256.Sum256([]byte(state))
		out.Verdicts = append(out.Verdicts, destructiveVerdict{
			Slot: slot, Label: d.Choice, Confidence: d.Conf, Question: q.Instructions,
			StateDigest: hex.EncodeToString(sum[:]), Fingerprint: fp, Retried: retried,
		})
		if !destructiveApproved(d) {
			return cancel("approval-cancelled")
		}
		if seen[fp] {
			return cancel("duplicate-verdict")
		}
		seen[fp] = true
	}
	if err := r.Run(); err != nil {
		return cancel("action-failed")
	}
	out.Approved = true
	return out
}
