package main

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

func TestCacheCanonicalDeterminism(t *testing.T) {
	state := `{"b":2,"a":{"z":"x","y":1},"arr":[3,{"n":0.10}]}`
	h1 := keyHash(canonicalState(state))
	h2 := keyHash(canonicalState(state))
	if h1 != h2 {
		t.Fatalf("same state hashed differently: %s vs %s", h1, h2)
	}

	// volatile fields change -> key identical
	stable := `{"task":"classify","session_id":"s-1","timestamp":1,"client":"claude"}`
	changed := `{"task":"classify","session-id":"s-99999","ts":987654321,"clientName":"cursor"}`
	if keyHash(canonicalState(stable)) != keyHash(canonicalState(changed)) {
		t.Fatal("volatile field changed the cache key")
	}

	// semantic change -> key differs
	sem := `{"task":"route","session_id":"s-1","timestamp":1,"client":"claude"}`
	if keyHash(canonicalState(stable)) == keyHash(canonicalState(sem)) {
		t.Fatal("semantic state change did not change the cache key")
	}

	// -0 fold
	if canonicalState(`{"n":-0}`) != canonicalState(`{"n":0}`) {
		t.Fatal("-0 not folded")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := newDecisionCache()
	c.cap = 2
	c.store("a", cachedDecision{Choice: "a"})
	c.store("b", cachedDecision{Choice: "b"})
	if _, ok := c.lookup("a"); !ok { // touch a -> b is now oldest
		t.Fatal("a missing before eviction")
	}
	c.store("c", cachedDecision{Choice: "c"})
	if _, ok := c.lookup("a"); !ok {
		t.Fatal("recently used cell evicted")
	}
	if _, ok := c.lookup("b"); ok {
		t.Fatal("oldest cell survived eviction")
	}
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
func TestWarmHitClearsRefreshFlag(t *testing.T) {
	c := newShardedCache()
	k := [32]byte{1}
	d := cachedDecision{Choice: "yes", ConfBucket: "high", Reason: "warm"}
	c.warmMu.Lock()
	c.warmHits[k] = 4
	c.warmAt[k] = time.Now()
	c.warmDec[k] = d
	c.warmExpires[k] = time.Now().Add(time.Hour)
	c.refreshing[k] = true
	c.warmMu.Unlock()

	got, ok := c.lookupForDecision(k)
	if !ok || got != d {
		t.Fatalf("warm hit = %+v, %v; want %+v, true", got, ok, d)
	}
	c.warmMu.Lock()
	refreshing := c.refreshing[k]
	c.warmMu.Unlock()
	if refreshing {
		t.Fatal("warm hit left the refresh flag set")
	}
}
