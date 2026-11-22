package main

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// decision_items.json is the 41-item decision registry (data only: each item
// carries the question, its label set, which labels may proceed, and whether
// it is irreversible or forbidden to ask about at all).
//
//go:embed decision_items.json
var decisionItemsJSON []byte

type decisionItem struct {
	ID           string            `json:"id"`
	Group        string            `json:"group"`
	Gate         bool              `json:"gate"`
	Irreversible bool              `json:"irreversible"`
	Provisional  bool              `json:"provisional"`
	Requirement  string            `json:"requirement"`
	StateFields  []string          `json:"state_fields"`
	Threshold    float64           `json:"threshold"` // 0 = use the global gate
	Q            decisionQ         `json:"q"`
	Proceed      []string          `json:"proceed"`
	Action       map[string]string `json:"action"`
}

type decisionQ struct {
	Instructions string            `json:"instructions"`
	Labels       map[string]string `json:"labels"`
}

var (
	decisionOnce  sync.Once
	decisionIndex map[string]decisionItem
)

func loadDecisionItems() map[string]decisionItem {
	decisionOnce.Do(func() {
		var f struct {
			Items []decisionItem `json:"items"`
		}
		decisionIndex = map[string]decisionItem{}
		if json.Unmarshal(decisionItemsJSON, &f) != nil {
			return // a broken bed is an empty registry: everything passes through
		}
		for _, it := range f.Items {
			decisionIndex[it.ID] = it
		}
	})
	return decisionIndex
}

func decisionItemFor(id string) (decisionItem, bool) {
	if id == "" {
		return decisionItem{}, false
	}
	it, ok := loadDecisionItems()[id]
	return it, ok
}

// admits is the registry's proceed list, not the model's opinion. A label
// outside the list never reaches the typed decision layer.
func (it decisionItem) admits(choice string) bool {
	for _, p := range it.Proceed {
		if p == choice {
			return true
		}
	}
	return false
}

// threshold clamps the item's own cut to the global gate: a measured per-item
// value may lower the bar, never raise it above the global rule.
func (it decisionItem) threshold(global float64) float64 {
	if it.Threshold > 0 && it.Threshold < global {
		return it.Threshold
	}
	return global
}

// question is the item's own question: instructions plus its exact label set,
// so an answer must be one of the item's labels (askSOQ enforces that).
func (it decisionItem) question() soQuestion {
	return soQuestion{Type: "choice", Instructions: it.Q.Instructions, Criteria: it.Q.Labels}
}
