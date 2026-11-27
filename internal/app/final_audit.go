package app

import (
	"encoding/json"
	"strconv"
	"strings"
)

// auditVerdict is the local final-answer check: OK only when every declared
// todo item is covered by a done entry and every named risk is verified by
// evidence. Missing carries the first few uncovered items for the report, never
// a model call. An unreadable state is NOT OK: an audit that cannot read the
// claim must not pass it.
type auditVerdict struct {
	OK      bool
	Missing []string
	Steer   string
}

type auditState struct {
	Todo            []string        `json:"todo"`
	Done            []string        `json:"done"`
	Risk            []string        `json:"risk"`
	Risks           []string        `json:"risks"`
	UnverifiedRisk  []string        `json:"unverified_risk"`
	UnverifiedRisks []string        `json:"unverified_risks"`
	Who             json.RawMessage `json:"who"`  // owner metadata, shape-agnostic
	When            json.RawMessage `json:"when"` // round metadata, shape-agnostic
}

// finalAudit checks a "done" claim against the state it came from. A todo
// entry prefixed "risk:" is a risk, so one list covers both. An unreadable
// state fails closed. Pure: no I/O, no network, no model.
func finalAudit(state string) auditVerdict {
	var s auditState
	if json.Unmarshal([]byte(state), &s) != nil {
		return auditVerdict{Missing: []string{"state unreadable"}, Steer: auditSteer([]string{"state unreadable"})}
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

// riskCovered accepts an exact done entry or a done entry that both names the
// risk and states evidence ("verified api timeout under load" covers "api
// timeout"). Naming alone is not coverage: a sentence that only restates the
// risk, or says it was not checked, covers nothing.
func riskCovered(key string, done map[string]bool) bool {
	for d := range done {
		// Judge the claim, not the risk name it quotes: a risk called
		// "pending migration" can never be mentioned without its deny word.
		if mentionsKey(d, key) && hasEvidence(d) && !deniesWork(strings.ReplaceAll(d, key, " ")) {
			return true
		}
	}
	return false
}

// mentionsKey requires the risk key to sit on word boundaries, so a short key
// like "test" is not covered by an unrelated "tested".
func mentionsKey(s, key string) bool {
	for idx := strings.Index(s, key); idx >= 0; {
		before := idx == 0 || !isWordByte(s[idx-1])
		end := idx + len(key)
		after := end == len(s) || !isWordByte(s[end])
		if before && after {
			return true
		}
		next := strings.Index(s[idx+1:], key)
		if next < 0 {
			return false
		}
		idx += 1 + next
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// hasEvidence reports whether a done entry states an evidence verb rather than
// merely restating the item.
func hasEvidence(s string) bool {
	for _, w := range []string{"verified", "tested", "fixed", "confirmed", "reproduced", "proved", "validated", "measured", "checked"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// deniesWork reports whether a done entry says the work was not actually done.
func deniesWork(s string) bool {
	for _, w := range []string{"not ", "n't", "never", "unverified", "untested", "unchecked", "unresolved", "skipped", "todo", "still open", "pending", "blocked", "no conclusion", "unknown", "open"} {
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
