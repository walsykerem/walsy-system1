package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// every decision point in the registry, driven end to end through route():
// a local item calls no model, a gated item asks with its own label set, an
// admitted label carries the item's action, a label outside the proceed list
// refuses with that label's own action, and a below-cut answer steers instead
// of blocking. This is the per-item evidence for "all points are gated" - one
// aggregate test over the whole registry, not a spot check of two items.
func TestEveryDecisionPointSteers(t *testing.T) {
	t.Setenv("EVREN_API_KEY", "sweep-test-key")
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		_, _ = io.ReadAll(r.Body)
		label, conf := answerFor(r.URL.Query().Get("k"))
		b, _ := json.Marshal(map[string]any{"answers": map[string]any{"gate": map[string]any{
			"choice": label, "confidence": conf,
		}}})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = srv.URL, srv.Client()
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()

	index := loadDecisionItems()
	if len(index) != 56 {
		t.Fatalf("registry has %d items, want exactly 56", len(index))
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	admitted, refused, low, lowNo, local, skipped := 0, 0, 0, 0, 0, 0
	for _, id := range ids {
		it := index[id]
		env := envelope(id, it.Gate)
		for label := range it.Q.Labels {
			if it.Action[label] == "" {
				t.Errorf("%s: label %q has no action to steer with", id, label)
			}
		}
		for _, p := range it.Proceed {
			if _, ok := it.Q.Labels[p]; !ok {
				t.Errorf("%s: proceed label %q is outside its own label set", id, p)
			}
		}

		if it.ID == "antishirk.todo_ledger" {
			// Deterministic ledger: the todo/done list is the decision, and no
			// model call may happen either way.
			atomic.StoreInt64(&calls, 0)
			open := route(envelopeWithState(id, `{"todo":["write tests"],"done":[]}`), nil, nil)
			done := route(envelopeWithState(id, `{"todo":["write tests"],"done":["write tests"]}`), nil, nil)
			if open.Reason != "ledger-open" || !strings.Contains(open.Steer, "write tests") {
				t.Errorf("ledger open = %+v, want ledger-open naming the open item", open)
			}
			if done.Reason != "ledger-complete" || done.Decision != decPin {
				t.Errorf("ledger complete = %+v, want ledger-complete pinned", done)
			}
			if n := atomic.LoadInt64(&calls); n != 0 {
				t.Errorf("ledger made %d model calls", n)
			}
			local++
			continue
		}
		if !it.Gate {
			atomic.StoreInt64(&calls, 0)
			setAnswer("", 0)
			r := route(env, nil, nil)
			if r.Reason != "registry-local" || r.Steer == "" {
				t.Errorf("%s: local item = %+v, want registry-local with a steer", id, r)
			}
			if n := atomic.LoadInt64(&calls); n != 0 {
				t.Errorf("%s: local item made %d model calls", id, n)
			}
			local++
			continue
		}

		proceed := sorted(it.Proceed)[0]
		rest := nonProceed(it)
		if rest == "" {
			// Nothing to refuse only if the item really admits every label it
			// defines; an empty label set would otherwise pass silently.
			if len(it.Q.Labels) == 0 {
				t.Errorf("%s: no labels to decide on", id)
			}
			for l := range it.Q.Labels {
				if !it.admits(l) {
					t.Errorf("%s: label %q left the proceed list but nonProceed missed it", id, l)
				}
			}
			skipped++
		}

		setAnswer(proceed, 0.95)
		if r := route(env, nil, nil); r.Reason != "so-admit" || r.Steer != it.Action[proceed] || r.Steer == "" {
			t.Errorf("%s: admit %q = %+v, want so-admit steering %q", id, proceed, r, it.Action[proceed])
		} else {
			admitted++
		}

		if rest != "" {
			setAnswer(rest, 0.95)
			if r := route(env, nil, nil); r.Reason != "registry-nonproceed" || r.Steer != it.Action[rest] || r.Steer == "" {
				t.Errorf("%s: non-proceed %q = %+v, want registry-nonproceed steering %q", id, rest, r, it.Action[rest])
			} else {
				refused++
			}
		}

		setAnswer(proceed, 0.30)
		r := route(env, nil, nil)
		// Below the cut the item still steers with its own action for the label
		// it returned, which is precise; a generic "scored below 0.8" is not.
		if r.Reason != "so-low-conf" || r.Label != proceed || r.Steer != it.Action[proceed] || r.Steer == "" {
			t.Errorf("%s: below-cut %q = %+v, want so-low-conf carrying the item action %q", id, proceed, r, it.Action[proceed])
		} else {
			low++
		}

		if rest != "" {
			// A non-proceed label is refused whatever its confidence: the label
			// already says stop, so weak evidence must not reopen the action.
			// It still steers with that label's own action, not generic text.
			setAnswer(rest, 0.30)
			if r := route(env, nil, nil); r.Reason != "registry-nonproceed" || r.Label != rest || r.Steer != it.Action[rest] || r.Steer == "" {
				t.Errorf("%s: low-confidence %q = %+v, want registry-nonproceed carrying %q", id, rest, r, it.Action[rest])
			} else {
				lowNo++
			}
		}

		setAnswer("outside-the-set", 0.95)
		if r := route(env, nil, nil); r.Reason != "so-degraded" || r.Steer == "" {
			t.Errorf("%s: out-of-set label = %+v, want so-degraded with a steer", id, r)
		}
	}
	if local+admitted+refused+low == 0 {
		t.Fatal("sweep asserted nothing")
	}
	t.Logf("points: %d local, %d admitted, %d refused, %d low-conf steered, %d low-confidence non-proceed still refused, %d with no non-proceed label",
		local, admitted, refused, low, lowNo, skipped)
	if local == 0 || admitted == 0 || refused == 0 || low == 0 || lowNo == 0 {
		t.Fatalf("coverage collapsed: %d/%d/%d/%d/%d", local, admitted, refused, low, lowNo)
	}
}

// The handler runs on the server goroutine, so the reply the test wants next is
// behind a mutex rather than a bare pair.
var (
	ansMu    sync.Mutex
	curLabel string
	curConf  float64
)

func setAnswer(label string, conf float64) {
	ansMu.Lock()
	defer ansMu.Unlock()
	curLabel, curConf = label, conf
}

func answerFor(string) (string, float64) {
	ansMu.Lock()
	defer ansMu.Unlock()
	return curLabel, curConf
}

func envelope(id string, gated bool) string {
	return envelopeWithState(id, "decide this step: "+id+" for the current task")
}

func envelopeWithState(id, text string) string {
	b, _ := json.Marshal(map[string]any{
		"state": text,
		"phase": "before-model", "authority": true, "opt_in": true,
		"calibrated": true, "need_label": true, "no_scratch": true,
		"safe": true, "reversible": true, "item": id,
	})
	return string(b)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// nonProceed returns a label the item does not admit, or "" when every label
// it knows is on the proceed list.
func nonProceed(it decisionItem) string {
	labels := make([]string, 0, len(it.Q.Labels))
	for l := range it.Q.Labels {
		labels = append(labels, l)
	}
	for _, l := range sorted(labels) {
		if !it.admits(l) {
			return l
		}
	}
	return ""
}

// TestEveryDecisionPointLive is the same sweep against the real endpoint: one
// model call per gated item, no stub. Opt-in so the default suite stays offline
// and deterministic; evidence runs set WALSY_LIVE=1. It asserts only what the
// router promises for any answer - a reason from the four routing branches, a
// label from the item's own set, and a steer taken from the registry - not which
// label the model picks.
func TestEveryDecisionPointLive(t *testing.T) {
	if os.Getenv("WALSY_LIVE") != "1" {
		t.Skip("live sweep: set WALSY_LIVE=1 and a provider key")
	}
	origEndpoint, origClient := soEndpoint, soClient
	soEndpoint, soClient = soEndpointDefault(), &http.Client{Timeout: 25 * time.Second}
	defer func() { soEndpoint, soClient = origEndpoint, origClient }()
	index := loadDecisionItems()
	if len(index) != 56 {
		t.Fatalf("registry has %d items, want exactly 56", len(index))
	}
	var admitted, refused, low, degraded, local int
	labels := map[string]int{}
	for _, id := range sorted(itemIDs(index)) {
		it := index[id]
		if !it.Gate {
			local++
			continue
		}
		r := route(envelope(id, true), nil, nil)
		labels[r.Label+"|"+r.Reason]++
		switch r.Reason {
		case "so-admit":
			admitted++
			if r.Decision != decPin {
				t.Errorf("%s: so-admit but decision %v", id, r.Decision)
			}
		case "registry-nonproceed":
			refused++
		case "so-low-conf":
			low++
		case "so-degraded", "so-timeout", "so-error":
			degraded++
		default:
			t.Errorf("%s: reason %q is not a routing branch (label %q)", id, r.Reason, r.Label)
			continue
		}
		if r.Label != "" {
			if _, ok := it.Q.Labels[r.Label]; !ok {
				t.Errorf("%s: label %q not in the item's own set", id, r.Label)
			}
		}
		if r.Reason != "so-degraded" && r.Steer != it.Action[r.Label] {
			t.Errorf("%s: steer %q is not the registry action for %q", id, r.Steer, r.Label)
		}
		if r.Reason != "so-admit" && r.Steer == "" {
			t.Errorf("%s: %s carries no steer", id, r.Reason)
		}
	}
	if admitted+refused+low+degraded != len(index)-local {
		t.Fatalf("covered %d items, want %d", admitted+refused+low+degraded, len(index)-local)
	}
	t.Logf("live points: %d admitted, %d refused, %d low-conf, %d degraded, %d local", admitted, refused, low, degraded, local)
	for k, n := range labels {
		t.Logf("  %3d  %s", n, k)
	}
}

func itemIDs(index map[string]decisionItem) []string {
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	return ids
}
