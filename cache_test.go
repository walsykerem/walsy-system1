package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func TestCacheNegativeVerdictIsAbstain(t *testing.T) {
	for _, choice := range []string{"no", "other"} {
		c := newDecisionCache()
		c.admit("k-"+choice, cachedDecision{Choice: choice, ConfBucket: "0.9"})
		d, ok := c.lookup("k-" + choice)
		if !ok {
			t.Fatalf("%s: expected negative cell cached", choice)
		}
		if d.Choice != abstainLabel {
			t.Fatalf("%s served as %q, want %q", choice, d.Choice, abstainLabel)
		}
	}
}

func TestCacheAdmissionBucket(t *testing.T) {
	// conf<0.8 (invalid bucket): never served as a positive label.
	c0 := newDecisionCache()
	c0.admit("low", cachedDecision{Choice: "yes", ConfBucket: ""})
	d0, ok := c0.lookup("low")
	if !ok || d0.Choice != abstainLabel {
		t.Fatalf("invalid bucket not stored as abstain: %+v ok=%v", d0, ok)
	}

	// valid bucket -> admitted.
	c1 := newDecisionCache()
	c1.admit("good", cachedDecision{Choice: "yes", ConfBucket: "0.8"})
	if d, ok := c1.lookup("good"); !ok || d.Choice != "yes" {
		t.Fatalf("valid decision not admitted: %+v ok=%v", d, ok)
	}
}

func TestCacheSingleflightCoalescing(t *testing.T) {
	var g group
	var calls int64
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]cachedDecision, 10)
	errs := make([]error, 10)

	// Leader registers then blocks inside fn, so followers deterministically join it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = g.do("same", func() (cachedDecision, error) {
			atomic.AddInt64(&calls, 1)
			close(started)
			<-release
			return cachedDecision{Choice: "yes"}, nil
		})
	}()
	<-started

	arrived := make(chan struct{}, 9)
	for i := 1; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arrived <- struct{}{}
			results[i], errs[i] = g.do("same", func() (cachedDecision, error) {
				atomic.AddInt64(&calls, 1)
				return cachedDecision{Choice: "wrong"}, nil
			})
		}(i)
	}
	for i := 1; i < 10; i++ {
		<-arrived
	}
	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("fn called %d times, want 1", got)
	}
	for i := range results {
		if errs[i] != nil || results[i].Choice != "yes" {
			t.Fatalf("caller %d: %+v err=%v", i, results[i], errs[i])
		}
	}
}

func TestCacheSingleflightPanicRecover(t *testing.T) {
	var g group
	_, err := g.do("panic", func() (cachedDecision, error) {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("panic not recovered as error: %v", err)
	}
}

func TestCacheSingleflightIdentityCheck(t *testing.T) {
	var g group
	c1 := &groupCall{}
	c2 := &groupCall{}
	g.m = map[string]*groupCall{"k": c1}
	g.finish("k", c2)
	g.mu.Lock()
	_, ok := g.m["k"]
	g.mu.Unlock()
	if !ok {
		t.Fatal("stale finish deleted a different call")
	}
	g.finish("k", c1)
	g.mu.Lock()
	_, ok = g.m["k"]
	g.mu.Unlock()
	if ok {
		t.Fatal("identity-matched finish did not delete call")
	}
}

func TestCacheSingleflightForgetStale(t *testing.T) {
	var g group
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		g.do("k", func() (cachedDecision, error) {
			close(oldStarted)
			<-releaseOld
			return cachedDecision{Choice: "old"}, nil
		})
	}()
	<-oldStarted
	g.forget("k")

	newStarted := make(chan struct{})
	releaseNew := make(chan struct{})
	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		g.do("k", func() (cachedDecision, error) {
			close(newStarted)
			<-releaseNew
			return cachedDecision{Choice: "new"}, nil
		})
	}()
	<-newStarted

	close(releaseOld)
	<-oldDone
	g.mu.Lock()
	_, stillThere := g.m["k"]
	g.mu.Unlock()
	if !stillThere {
		t.Fatal("stale leader finish removed the new in-flight call")
	}
	close(releaseNew)
	<-newDone
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

func TestCacheTwoSeparateCounters(t *testing.T) {
	c := newDecisionCache()
	c.admit("k", cachedDecision{Choice: "yes", ConfBucket: "0.9"})
	c.lookup("k")
	c.recordProviderPrefix(512, 2048)

	router := c.stats()
	kv := c.providerStats()
	if router.Hits != 1 {
		t.Fatalf("router ledger wrong: %+v", router)
	}
	if kv.MatchPrefix != 1 || kv.PrefixLen != 512 || kv.PromptLen != 2048 {
		t.Fatalf("provider KV ledger wrong: %+v", kv)
	}
	// Type-level separation: router stats have no prefix fields, provider stats
	// have no hit fields (compile-time guarantee).
	_ = cacheStats{}
	_ = providerKVStats{MatchPrefix: 0}
	_ = fmt.Sprintf("%T %T", router, kv)
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
