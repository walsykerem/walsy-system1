package app

import (
	"strconv"
	"sync"
)

// admitReasonsCap bounds the coarse reason baseline kept for operator feedback.
const admitReasonsCap = 16

// admitWindow counts admitted vs blocked decisions in memory. Only coarse
// counts and short reason codes reach routerAudit/health; no raw confidence or
// label is kept or logged.
type admitWindow struct {
	mu      sync.Mutex
	n       int
	admits  int
	blocked int
	reasons map[string]int
}

// admitCounts is a mutex-safe copy for health/operator surfaces.
type admitCounts struct {
	N, Admits, Blocked int
	Reasons            map[string]int
}

// record counts one decision. reason is optional and must be a coarse code;
// raw labels and confidence never enter the baseline.
func (w *admitWindow) record(admitted bool, reason ...string) {
	w.mu.Lock()
	w.n++
	if admitted {
		w.admits++
	} else {
		w.blocked++
	}
	if len(reason) > 0 {
		r := reason[0]
		if len(r) > 32 {
			r = r[:32]
		}
		if r != "" && len(w.reasons) < admitReasonsCap {
			if w.reasons == nil {
				w.reasons = make(map[string]int, admitReasonsCap)
			}
			w.reasons[r]++
		}
	}
	emit := w.n%100 == 0
	n, admits, blocked := w.n, w.admits, w.blocked
	w.mu.Unlock()
	if !emit {
		return
	}
	routerAudit("admit_rate", "count", strconv.Itoa(admits),
		"status", "window100", "reason", strconv.Itoa(blocked))
	cfg := readSOConfig()
	base, err := strconv.ParseFloat(cfg["admit_baseline"], 64)
	if err != nil {
		base = 0.8
	}
	tol, err := strconv.ParseFloat(cfg["drift_tolerance"], 64)
	if err != nil {
		tol = 0.1
	}
	rate := float64(admits) / float64(n)
	if rate < base-tol || rate > base+tol {
		routerAudit("drift_alarm", "count", strconv.Itoa(admits),
			"status", "window100", "reason", "admit_baseline")
	}
}

// snapshot returns a copy of the window counters under the mutex.
func (w *admitWindow) snapshot() admitCounts {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := admitCounts{N: w.n, Admits: w.admits, Blocked: w.blocked,
		Reasons: make(map[string]int, len(w.reasons))}
	for k, v := range w.reasons {
		out.Reasons[k] = v
	}
	return out
}

// admitStats is the process-wide admit/block window. No persistence, no TTL.
var admitStats = &admitWindow{}
