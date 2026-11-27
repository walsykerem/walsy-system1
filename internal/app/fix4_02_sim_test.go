package app

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

func fix402Key() [32]byte {
	var k [32]byte
	k[0], k[1] = 7, 3
	return k
}

func fix402Refreshing(c *shardedCache, k [32]byte) bool {
	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	return c.refreshing[k]
}

func fix402WaitRefresh(c *shardedCache, k [32]byte) bool {
	until := time.Now().Add(3 * time.Second)
	for fix402Refreshing(c, k) && time.Now().Before(until) {
		runtime.Gosched()
	}
	return !fix402Refreshing(c, k)
}

// Distinct large JSON integers must not collapse to one float64 and share a
// decision cell: that would serve one state's decision to another state.
func TestFix402LargeNumberKeySeparation(t *testing.T) {
	a := canonicalState(`{"n":9007199254740992}`)
	b := canonicalState(`{"n":9007199254740993}`)
	if a == b {
		t.Fatal("float64 key normalization collapsed two distinct large integers")
	}
	for _, pair := range [][2]string{
		{`{"n":1}`, `{"n":1.0}`},
		{`{"n":1}`, `{"n":1e0}`},
		{`{"n":-0}`, `{"n":0}`},
	} {
		if x, y := canonicalState(pair[0]), canonicalState(pair[1]); x != y {
			t.Fatalf("equivalent numbers did not normalize: %s != %s", pair[0], pair[1])
		}
	}
	cap := soCapability()
	r1 := []byte(`{"params":{"n":9007199254740992}}`)
	r2 := []byte(`{"params":{"n":9007199254740993}}`)
	if gateKey(r1, cap) == gateKey(r2, cap) {
		t.Fatal("gateKey reused a decision cell across distinct large integers")
	}
}

// A warm fallback may coast to the grace deadline, never beyond it: restoring
// the cell must keep that deadline instead of minting a fresh full TTL.
func TestFix402WarmRestoreKeepsGraceDeadline(t *testing.T) {
	c := newShardedCache()
	k := fix402Key()
	deadline := time.Now().Add(warmStaleGrace)
	warm := cachedDecision{Choice: "yes", Label: "warm"}
	c.warmHits[k], c.warmAt[k] = 4, time.Now()
	c.warmDec[k], c.warmExpires[k] = warm, deadline
	c.refreshing[k] = true

	d, ok := c.lookupForDecision(k)
	if !ok || d.Label != "warm" {
		t.Fatalf("warm hit = %+v/%v, want the prewarmed decision", d, ok)
	}
	if c.refreshing[k] {
		t.Fatal("warm hit left the refresh flag set")
	}
	cell := cellFor(c, k)
	if cell == nil {
		t.Fatal("warm restore did not restore the cell")
	}
	if !cell.expiresAt.Equal(deadline) {
		t.Fatalf("warm restore expiry = %v, want grace deadline %v", cell.expiresAt, deadline)
	}
	if _, ok := c.lookup(k); !ok {
		t.Fatal("warm restore did not restore the cell")
	}
}

// A hot-key refresh is a flag handoff, not a new decision: copying the cell on
// a hit must not extend its TTL.
func TestFix402HotHitDoesNotExtendTTL(t *testing.T) {
	c := newShardedCache()
	k := fix402Key()
	c.store(k, cachedDecision{Choice: "yes", Label: "admit"})
	want := cellFor(c, k).expiresAt
	c.warmHits[k], c.warmAt[k] = 2, time.Now()

	if _, ok := c.lookupForDecision(k); !ok {
		t.Fatal("hot crossing lost the live cell")
	}
	if !fix402WaitRefresh(c, k) {
		t.Fatal("hot crossing left the refresh flag set")
	}
	if got := cellFor(c, k).expiresAt; !got.Equal(want) {
		t.Fatalf("hot hit extended cell TTL: %v -> %v", want, got)
	}
}

// A newer terminal decision invalidates the warm copy. Otherwise a later
// expiry would serve the superseded admit from warm state.
func TestFix402StoreInvalidatesWarmDecision(t *testing.T) {
	c := newShardedCache()
	k := fix402Key()
	stale := cachedDecision{Choice: "yes", Label: "stale-admit"}
	c.store(k, stale)
	c.warmHits[k], c.warmAt[k] = 4, time.Now()
	c.warmDec[k], c.warmExpires[k] = stale, time.Now().Add(time.Minute)

	c.store(k, cachedDecision{Abstain: true, Reason: "so-no", Label: "no"})
	cellFor(c, k).expiresAt = time.Now().Add(-time.Second)

	if d, ok := c.lookupForDecision(k); ok {
		t.Fatalf("stale warm decision served after store: %+v", d)
	}
	if _, ok := c.warmDec[k]; ok {
		t.Fatal("warm copy survived invalidation")
	}
}

func BenchmarkFix402CanonicalState(b *testing.B) {
	s := `{"params":{"state":"classify this request","n":9007199254740993}}`
	for i := 0; i < b.N; i++ {
		_ = canonicalState(s)
	}
}

func BenchmarkFix402CacheHit(b *testing.B) {
	c := newShardedCache()
	k := fix402Key()
	c.store(k, cachedDecision{Choice: "yes", Label: "admit"})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.lookupForDecision(k)
	}
}

// Concurrent identical lookups coalesce into one refresh owner and never
// extend the cell TTL.
func TestFix402ConcurrentHotHitsSingleOwner(t *testing.T) {
	c := newShardedCache()
	k := fix402Key()
	c.store(k, cachedDecision{Choice: "yes", Label: "admit"})
	want := cellFor(c, k).expiresAt
	c.warmHits[k], c.warmAt[k] = 2, time.Now()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := c.lookupForDecision(k); !ok {
				t.Error("concurrent hot hit missed a live cell")
			}
		}()
	}
	close(start)
	wg.Wait()
	if !fix402WaitRefresh(c, k) {
		t.Fatal("concurrent hot hits left the refresh flag set")
	}
	if got := cellFor(c, k).expiresAt; !got.Equal(want) {
		t.Fatalf("concurrent hot hits extended cell TTL: %v -> %v", want, got)
	}
}
