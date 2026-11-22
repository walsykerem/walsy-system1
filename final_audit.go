package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// auditVerdict is the local final-answer check: OK only when every declared
// todo item is covered by a done entry and every named risk is verified.
// Missing carries the first few uncovered items for the report, never a model
// call. An unreadable state is OK: a malformed block must not block a report.
type auditVerdict struct {
	OK      bool
	Missing []string
	Steer   string
}

type auditState struct {
	Todo            []string `json:"todo"`
	Done            []string `json:"done"`
	Risk            []string `json:"risk"`
	Risks           []string `json:"risks"`
	UnverifiedRisk  []string `json:"unverified_risk"`
	UnverifiedRisks []string `json:"unverified_risks"`
}

// finalAudit checks a "done" claim against the state it came from. A todo
// entry prefixed "risk:" is a risk, so one list covers both. Fail-open on
// unreadable state. Pure: no I/O, no network, no model.
func finalAudit(state string) auditVerdict {
	var s auditState
	if json.Unmarshal([]byte(state), &s) != nil {
		return auditVerdict{OK: true}
	}
	done := map[string]bool{}
	for _, d := range s.Done {
		if k := ledgerKey(d); k != "" {
			done[k] = true
		}
	}
	var todos, risks []string
	for _, t := range s.Todo {
		k := ledgerKey(t)
		if k == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(k, "risk:"); ok {
			risks = append(risks, strings.TrimSpace(rest))
			continue
		}
		todos = append(todos, strings.TrimSpace(t))
	}
	for _, list := range [][]string{s.Risk, s.Risks, s.UnverifiedRisk, s.UnverifiedRisks} {
		for _, r := range list {
			if k := ledgerKey(r); k != "" {
				risks = append(risks, strings.TrimSpace(r))
			}
		}
	}
	var missing []string
	seen := map[string]bool{}
	for _, t := range todos {
		k := ledgerKey(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		if !done[k] {
			missing = append(missing, t)
		}
	}
	for _, r := range risks {
		k := ledgerKey(r)
		if k == "" || seen["risk:"+k] {
			continue
		}
		seen["risk:"+k] = true
		if !riskCovered(k, done) {
			missing = append(missing, "risk: "+r)
		}
	}
	if len(missing) == 0 {
		return auditVerdict{OK: true}
	}
	return auditVerdict{OK: false, Missing: missing, Steer: auditSteer(missing)}
}

// riskCovered accepts an exact done entry or a done entry that names the risk,
// so "verified api timeout under load" covers "api timeout". A done entry that
// denies the wording ("did not verify api timeout") covers nothing: the point is
// the evidence, and a sentence that says the risk was not checked is not it.
func riskCovered(key string, done map[string]bool) bool {
	if done[key] {
		return true
	}
	for d := range done {
		if strings.Contains(d, key) && !deniesWork(d) {
			return true
		}
	}
	return false
}

// deniesWork reports whether a done entry says the work was not actually done.
func deniesWork(s string) bool {
	for _, w := range []string{"not ", "n't", "never", "unverified", "untested", "unchecked", "skipped", "todo"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// auditSteer names the work still owed in one concrete next action.
func auditSteer(missing []string) string {
	shown := missing
	if len(shown) > 3 {
		shown = shown[:3]
	}
	rest := ""
	if n := len(missing) - len(shown); n > 0 {
		rest = " (+" + strconv.Itoa(n) + " more)"
	}
	return "do not claim done: finish or verify " + strings.Join(shown, "; ") + rest + ", then audit again."
}
