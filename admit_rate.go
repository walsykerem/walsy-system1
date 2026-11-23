package main

import (
	"strconv"
	"sync"
)

// admitWindow counts admitted vs blocked decisions in memory. Only the coarse
// count and a status label reach routerAudit; no raw confidence or label is
// kept or logged.
type admitWindow struct {
	mu      sync.Mutex
	n       int
	admits  int
	blocked int
}

// record counts one decision and emits a bounded window summary every 100.
func (w *admitWindow) record(admitted bool) {
	w.mu.Lock()
	w.n++
	if admitted {
		w.admits++
	} else {
		w.blocked++
	}
	emit := w.n%100 == 0
	admits, blocked := w.admits, w.blocked
	w.mu.Unlock()
	if emit {
		routerAudit("admit_rate", "count", strconv.Itoa(admits),
			"status", "window100", "reason", strconv.Itoa(blocked))
	}
}

// admitStats is the process-wide admit/block window. No persistence, no TTL.
var admitStats = &admitWindow{}
