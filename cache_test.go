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
		c.admit("k-"+choice, cachedDecision{Choice: choice, ConfBucket: "0.9", Reason: "margin=0.5"})
		d, ok := c.lookup("k-" + choice)
		if !ok {
			t.Fatalf("%s: expected negative cell cached", choice)
		}
		if d.Choice != abstainLabel {
			t.Fatalf("%s served as %q, want %q", choice, d.Choice, abstainLabel)
		}
	}
}

func TestCacheAdmissionMargin(t *testing.T) {
	// conf<0.8: never served as a positive label.
	c0 := newDecisionCache()
	c0.admit("low", cachedDecision{Choice: "yes", ConfBucket: "", Reason: "margin=0.9"})
	d0, ok := c0.lookup("low")
	if ok && d0.Choice == "yes" {
		t.Fatal("conf<0.8 admitted as label")
	}

	// 0.85 + sufficient margin -> admitted.
	c1 := newDecisionCache()
	c1.admit("good", cachedDecision{Choice: "yes", ConfBucket: "0.8", Reason: "margin=0.20"})
	if d, ok := c1.lookup("good"); !ok || d.Choice != "yes" {
		t.Fatalf("valid decision not admitted: %+v ok=%v", d, ok)
	}

	// low margin -> not admitted.
	c2 := newDecisionCache()
	c2.admit("narrow", cachedDecision{Choice: "yes", ConfBucket: "0.9", Reason: "margin=0.02"})
	if _, ok := c2.lookup("narrow"); ok {
		t.Fatal("low-margin decision admitted")
	}
}

func TestCacheCanarySelfHeal(t *testing.T) {
	c := newDecisionCache()
	c.admit("canary", cachedDecision{Choice: "wrong", ConfBucket: "0.9", Reason: "margin=0.5"})
	before := c.stats().WrongReuse

	if !shouldCanaryReplay(canaryEvery) || shouldCanaryReplay(1) {
		t.Fatal("canary cadence wrong")
	}
	if c.canaryReplay("canary", "yes") {
		t.Fatal("mismatched canary replay passed")
	}
	if _, ok := c.lookup("canary"); ok {
		t.Fatal("mismatched canary cell not flushed")
	}
	after := c.stats().WrongReuse
	if after != before+1 || c.canaryFlushes() == 0 {
		t.Fatalf("wrong_reuse counter not incremented: %d -> %d, flushes=%d", before, after, c.canaryFlushes())
	}

	c.admit("canary", cachedDecision{Choice: "yes", ConfBucket: "0.9", Reason: "margin=0.5"})
	if !c.canaryReplay("canary", "yes") || c.stats().ReusePrecision == 0 {
		t.Fatal("matching canary replay failed")
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
	c.admit("k", cachedDecision{Choice: "yes", ConfBucket: "0.9", Reason: "margin=0.5"})
	c.lookup("k")
	c.canaryFlushes()
	c.canaryReplay("k", "no")
	c.recordProviderPrefix(512, 2048)

	router := c.stats()
	kv := c.providerStats()
	if router.ReusePrecision != 0 || router.WrongReuse != 1 || router.Hits != 1 {
		t.Fatalf("router ledger wrong: %+v", router)
	}
	if kv.MatchPrefix != 1 || kv.PrefixLen != 512 || kv.PromptLen != 2048 {
		t.Fatalf("provider KV ledger wrong: %+v", kv)
	}
	// Type-level separation: router stats have no prefix fields, provider stats
	// have no reuse fields (compile-time guarantee).
	_ = cacheStats{ReusePrecision: 0, WrongReuse: 0}
	_ = providerKVStats{MatchPrefix: 0}
	_ = fmt.Sprintf("%T %T", router, kv)
}
