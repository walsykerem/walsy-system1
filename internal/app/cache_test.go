package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func keyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestDecisionCacheConcurrent is the race-detector proof: 8 goroutines hit
// store+lookup on overlapping keys, past capacity, and the invariant holds.
func TestDecisionCacheConcurrent(t *testing.T) {
	c := newDecisionCache()
	const goroutines, iters = 8, 200
	var wg sync.WaitGroup
	var overflow atomic.Bool
	d := cachedDecision{Choice: "yes", ConfBucket: "0.9"}

	check := func() {
		c.mu.Lock()
		n := len(c.idx)
		c.mu.Unlock()
		if n > c.cap {
			overflow.Store(true)
		}
	}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c.store(fmt.Sprintf("k-%d-%d", g, i), d) // 1600 keys > cap: eviction runs
				c.lookup(fmt.Sprintf("k-%d-%d", g, i/2))
				c.lookup("shared")
				check()
			}
		}(g)
	}
	wg.Wait()

	if overflow.Load() {
		t.Fatalf("cache exceeded capacity %d", c.cap)
	}
	if n := len(c.idx); n > c.cap {
		t.Fatalf("final size %d > cap %d", n, c.cap)
	}
}

// A warm hit serves the prewarmed decision without launching an async refresh,
// so it must clear the in-flight flag set by the prewarm threshold.
