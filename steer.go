package main

import "strings"

// SteerText is the one concrete next action for a relay reason-code, so a
// below-threshold verdict does not send the agent back to rediscover the
// problem. It never invites the same call again, and a refusal stays a refusal.
func SteerText(reason, label string) string {
	switch reason {
	case "registry-nonproceed":
		if label != "" {
			return "label \"" + label + "\" is outside the item's proceed list: take the item's action for it or hand it to a human."
		}
	case "so-low-conf":
		if label != "" {
			return "label \"" + label + "\" scored below 0.8: name the missing evidence and decide locally."
		}
	}
	if s := steerTable[reason]; s != "" {
		return s
	}
	// Any other non-proceed label: so-<choice>. The refusal stands.
	if r, ok := strings.CutPrefix(reason, "so-"); ok && r != "" {
		return "the model's " + r + " label is not an admit: take the reversible branch or hand to a human; the same state will get the same answer."
	}
	return ""
}

var steerTable = map[string]string{
	"layer0-opt-in":          "add the decision capability to the request: opt_in true or capability.<name>.enabled.",
	"layer0-scope":           "add the missing scope evidence: authority, opt_in, calibrated and phase \"before-model\".",
	"layer0-gate":            "fill the gate fields (need_label, no_scratch, safe, reversible) and set phase \"before-model\".",
	"layer0-static":          "answer this static request directly; no model decision is needed.",
	"layer0-shape":           "restate the state as one label-shaped decision: classify, score, route or a yes/no question.",
	"registry-local":         "decide locally from the registry item's proceed list; no model call is needed.",
	"registry-nonproceed":    "take the registry item's action for that label or hand it to a human.",
	"cache-hit":              "proceed with the cached admitted label; nothing is left to ask.",
	"so-degraded":            "name the label set and the concrete options, or decide locally; the model returned no usable label.",
	"so-low-conf":            "the evidence is too thin for a confident label: name the missing fact and decide locally.",
	"so-no":                  "not admitted: the typed layer passes this through unchanged, so keep going on your own judgment.",
	"so-other":               "no side picked: the typed layer passes this through unchanged, so keep going on your own judgment.",
	"so-admit":               "proceed with the admitted label.",
	"timeout":                "the decision model did not answer in time: decide locally from the evidence already held and note it was unavailable.",
	"no-applicable-question": "this item has no applicable question for the supplied state: provide the item's required state fields, or decide locally.",
	"transient":              "the decision model was transiently unavailable: proceed on your own judgment and note it was unreachable.",
	"permanent":              "the decision model rejected the request: fix the label set and state, or decide locally.",
	"unknown":                "the decision model gave no usable answer: decide locally from the evidence already held.",
	"ok":                     "proceed with the admitted label.",
	"ledger-complete":        "proceed: every declared todo item is covered by a done claim.",
	"ledger-open":            "finish the uncovered todo items or drop them explicitly, then re-check the ledger.",
	"no-state":               "send a JSON state object with the decision text under \"state\".",
	"frame-too-large":        "split the request into frames under 8 MiB before sending it again.",
	"no-key":                 "configure the relay's model key, or decide locally.",
	"unread-write":           "read the target path first, or state why it is new, then write.",
	"unread-path":            "give a readable target path, or state why it is new, then write; the guard could not read this path.",
	"repeat":                 "change the arguments or say why the call must repeat; the identical call will not be sent again.",
	"guard":                  "read the target first or justify the write; the guard refused it.",
}
