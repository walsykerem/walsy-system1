package app

import (
	"strconv"
	"strings"
)

// attemptRevertAt is the number of failed fix attempts on one failure
// signature before the verdict is revert. No flag, no config file.
const attemptRevertAt = 3

// attemptGuard counts failed fix attempts per failure signature. The caller
// owns one guard per task; the guard holds no globals and does no I/O. A
// signature comes from failureSignature, so the same failing test or build
// error keeps one counter across edits.
type attemptGuard struct {
	counts map[string]int
}

func newAttemptGuard() *attemptGuard {
	return &attemptGuard{counts: map[string]int{}}
}

// fail records one failed fix attempt and returns the revert verdict on the
// attempt that reaches the threshold. The counter for that signature is reset
// so a revert cannot immediately trip again; the caller must restore before
// trying the same failure a fourth time.
func (g *attemptGuard) fail(signature string) (revert bool, steer string) {
	sig := strings.TrimSpace(signature)
	if sig == "" {
		return false, ""
	}
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[sig]++
	if g.counts[sig] < attemptRevertAt {
		return false, ""
	}
	g.counts[sig] = 0
	return true, "revert: " + strconv.Itoa(attemptRevertAt) + " fix attempts failed on the same failure (" + shortSignature(sig) + "); restore the last known-good state and change approach, do not resend the same patch."
}

// ok clears a signature after a pass, so an unrelated later failure does not
// inherit old attempts.
func (g *attemptGuard) ok(signature string) {
	delete(g.counts, strings.TrimSpace(signature))
}

// attempts reports the count for one signature; tests and audit use it.
func (g *attemptGuard) attempts(signature string) int {
	return g.counts[strings.TrimSpace(signature)]
}

func shortSignature(sig string) string {
	const n = 16
	if len(sig) <= n {
		return sig
	}
	return sig[:n]
}
