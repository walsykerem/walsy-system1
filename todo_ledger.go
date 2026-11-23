package main

import (
	"encoding/json"
	"strings"
)

// todoLedger is the local antishirk check: the "todo" items a state declares
// against the "done" claims it carries. A named risk is a todo entry prefixed
// "risk:", so one list covers work and risk. It never calls a model, and a
// state it cannot read yields no verdict: a malformed block must not block
// work (fail open).
type todoLedger struct {
	Uncovered []string
	Verdict   soDecision // complete when Uncovered is empty, else do_more; Conf is the covered fraction
}

type todoLedgerState struct {
	Todo []string        `json:"todo"`
	Done []string        `json:"done"`
	Who  json.RawMessage `json:"who"`  // owner metadata, shape-agnostic
	When json.RawMessage `json:"when"` // round metadata, shape-agnostic
}

// ledgerKey folds case and whitespace so "Write  tests" matches "write tests".
func ledgerKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// todoLedgerFrom reads the optional "todo" and "done" string arrays from a
// JSON object state. No todo means no opinion: Verdict.Choice stays empty.
func todoLedgerFrom(state string) todoLedger {
	var s todoLedgerState
	if json.Unmarshal([]byte(state), &s) != nil {
		return todoLedger{}
	}
	done := make(map[string]bool, len(s.Done))
	for _, d := range s.Done {
		if k := ledgerKey(d); k != "" {
			done[k] = true
		}
	}
	var uncovered []string
	seen := make(map[string]bool, len(s.Todo))
	for _, t := range s.Todo {
		k := ledgerKey(t)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if !done[k] {
			uncovered = append(uncovered, strings.TrimSpace(t))
		}
	}
	if len(seen) == 0 {
		return todoLedger{}
	}
	choice := "complete"
	if len(uncovered) > 0 {
		choice = "do_more"
	}
	covered := len(seen) - len(uncovered)
	return todoLedger{
		Uncovered: uncovered,
		Verdict:   soDecision{Choice: choice, Conf: float64(covered) / float64(len(seen))},
	}
}
