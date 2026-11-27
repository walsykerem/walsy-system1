package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
// so it must clear the in-flight flag set by the prewarm threshold, and the
// coast ends at the grace deadline instead of pinning the key forever.
func TestCacheWarmFallbackBoundedByGrace(t *testing.T) {
	c := newShardedCache()
	var k [32]byte
	k[0] = 7
	warm := cachedDecision{Choice: decPass, Label: "warm"}
	c.warmHits[k], c.warmAt[k] = 4, time.Now()
	c.warmDec[k], c.warmExpires[k] = warm, time.Now().Add(warmStaleGrace)
	c.refreshing[k] = true // an earlier crossing already owns the refresh

	d, ok := c.lookupForDecision(k)
	if !ok || d.Label != "warm" {
		t.Fatalf("warm hit inside grace = %+v/%v, want the prewarmed decision", d, ok)
	}
	if c.refreshing[k] {
		t.Fatal("warm hit left the refresh flag set: the key can never prewarm again")
	}
	if _, ok := c.lookup(k); !ok {
		t.Fatal("warm hit did not restore the cell")
	}

	var k2 [32]byte
	k2[0] = 8
	c.warmHits[k2], c.warmAt[k2] = 4, time.Now()
	c.warmDec[k2], c.warmExpires[k2] = warm, time.Now().Add(-time.Second)
	if _, ok := c.lookupForDecision(k2); ok {
		t.Fatal("warm decision served past the grace deadline: the coast is not bounded")
	}
	if _, ok := c.warmDec[k2]; ok {
		t.Fatal("expired warm decision was kept")
	}
	if c.refreshing[k2] {
		t.Fatal("dropped warm hit left the refresh flag set")
	}
}
