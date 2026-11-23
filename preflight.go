package main

import (
	"encoding/json"
	"sync"
)

// preflightVerdict is a local, advisory shadow of the routing decision. It
// never feeds admission. Kept deliberately small: no probabilities, no raw
// prompt text, no secret.
type preflightVerdict struct {
	Reason     string
	ConfBucket string
	Ambiguous  bool
}

// preflight is a pure local rule tree over an already-built state envelope.
func preflight(e stateEnvelope) preflightVerdict {
	if !inScope(e) {
		return preflightVerdict{Reason: "layer0-scope"}
	}
	if staticShortCircuit(e.State) {
		return preflightVerdict{Reason: "layer0-static"}
	}
	if !isLabelShaped(e.State) {
		return preflightVerdict{Reason: "layer0-shape"}
	}
	it, ok := decisionItemFor(e.Item)
	if ok && !it.Gate {
		return preflightVerdict{Reason: "registry-local"}
	}
	// Decision-shaped work with no complete opt-in set is ambiguous; the real
	// route still decides. This only labels the shadow record.
	ambiguous := !e.NeedLabel || !e.NoScratch || !e.Safe || !e.Reversible ||
		!e.Authority || !e.Calibrated || !e.OptIn || e.Phase == ""
	return preflightVerdict{Reason: "so-pending", Ambiguous: ambiguous}
}

// preflightCalibration is an in-memory ring of shadow-vs-final reason pairs.
// Bounded, mutex-guarded, never persisted and never exposed as raw confidence.
type preflightCalibration struct {
	mu     sync.Mutex
	items  [256]string
	n      int
	next   int
	counts map[string]int
}

func newPreflightCalibration() *preflightCalibration {
	return &preflightCalibration{counts: make(map[string]int)}
}

func (c *preflightCalibration) add(pre, final string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pair := pre + "|" + final
	if c.n < len(c.items) {
		c.items[c.next] = pair
		c.n++
	} else {
		delete(c.counts, c.items[c.next])
		c.items[c.next] = pair
	}
	c.counts[pair]++
	c.next = (c.next + 1) % len(c.items)
}

// recordPreflight emits the advisory shadow metric/audit AFTER r is obtained.
// Admission is untouched: nothing from p is ever copied into r.
func recordPreflight(p preflightVerdict, r routeResult, ring *preflightCalibration) {
	reason := p.Reason
	if p.Ambiguous {
		reason = "ambiguous"
	}
	ring.add(reason, r.Reason)
	emitMetric(metric{Seq: 1, PID: 0, Route: "preflight", Decision: r.Decision,
		Reason: reason, Covered: r.Covered, ConfBucket: r.ConfBucket, Source: "preflight"})
	routerAudit("preflight", "source", "preflight", "reason", reason, "decision", r.Decision)
}

// preflightFromState builds an envelope from a state string; malformed state is
// an out-of-scope shadow, never an error that changes the real decision.
func preflightFromState(state string) preflightVerdict {
	var e stateEnvelope
	if json.Unmarshal([]byte(state), &e) != nil {
		return preflightVerdict{Reason: "layer0-scope"}
	}
	return preflight(e)
}
