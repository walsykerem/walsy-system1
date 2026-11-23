package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReceiptHashChain proves the persisted chain links every receipt: a
// recomputation from the first line must reproduce each recorded head.
func TestReceiptHashChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &receiptChain{}
	h1, err := c.appendReceipt(receipt{Event: "decision", Decision: decPin, Reason: "so-admit", Method: "tools/call"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := c.appendReceipt(receipt{Event: "decision", Decision: decPass, Reason: "so-no"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, routerDir, "receipt.chain")
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0700 {
		t.Fatalf("receipt dir perm = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("receipt file perm = %o, want 600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("receipt lines = %d, want 2", len(lines))
	}
	var prev []byte
	for i, line := range lines {
		var r receipt
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if r.ResponseID != "none" {
			t.Fatalf("receipt %d response_id = %q, want none", i, r.ResponseID)
		}
		if r.Nonce == "" || r.Seq != i+1 || r.Prev != hex.EncodeToString(prev) {
			t.Fatalf("receipt %d bad nonce/seq/prev: %+v", i, r)
		}
		body, err := canonicalJSON(r)
		if err != nil {
			t.Fatal(err)
		}
		sum := chainHash(prev, body)
		want := []string{h1, h2}[i]
		if hex.EncodeToString(sum[:]) != want {
			t.Fatalf("receipt %d chain head mismatch: recomputed %x, stored %s", i, sum, want)
		}
		prev = sum[:]
	}
	if bytes.Contains(raw, []byte("NEVER")) || bytes.Contains(raw, []byte("audit.jsonl")) {
		t.Fatal("receipt chain leaked a fabricated id or audit.jsonl name")
	}
}

// TestYC7Requires: Requires is a local-only declaration and must never reach
// the wire. No shipped producer emits untrusted_text/tool_output, so SO calls
// are NOT gated on it.
func TestYC7Requires(t *testing.T) {
	b, err := json.Marshal(soQuestion{Type: "choice", Instructions: "pick", Criteria: map[string]string{"yes": "go"}, Requires: []string{"untrusted_text", "tool_output"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("requires")) || bytes.Contains(b, []byte("untrusted_text")) || bytes.Contains(b, []byte("tool_output")) {
		t.Fatalf("Requires leaked onto the SO wire: %s", b)
	}
	// A normal decision item never gains a Requires gate: SO calls go through.
	q := decisionItem{Gate: true, Q: decisionQ{Instructions: "pick", Labels: map[string]string{"yes": "go"}}}.question()
	if bytes.Contains([]byte(q.Type), []byte("requires")) {
		t.Fatal("decision item question gated SO on Requires")
	}
}

// TestYC8StrictDecode rejects duplicate/case-variant keys, two documents, an
// oversized frame and unknown top-level keys instead of last-wins parsing.
func TestYC8StrictDecode(t *testing.T) {
	cases := map[string]string{
		"duplicate":    `{"answers":{},"answers":{}}`,
		"case-variant": `{"answers":{},"ANSWERS":{}}`,
		"two-docs":     `{"answers":{}} {"answers":{}}`,
		"unknown":      `{"answers":{},"routed_model":"other"}`,
	}
	for name, raw := range cases {
		if _, err := strictObject([]byte(raw), 1<<20, map[string]bool{"model": true, "answers": true, "usage": true}); err == nil {
			t.Errorf("%s: strict SO decode accepted %s", name, raw)
		}
	}
	if _, err := strictObject([]byte(`{"answers":{},"model":"clef"}`), 1<<20, map[string]bool{"model": true, "answers": true, "usage": true}); err != nil {
		t.Fatalf("valid SO answer rejected: %v", err)
	}
	if _, err := strictObject(bytes.Repeat([]byte("a"), 1<<20), 1<<20, nil); err == nil {
		t.Fatal("oversized SO body accepted")
	}
	if _, err := decodeFrame([]byte(`{"method":"x","METHOD":"y"}`)); err == nil {
		t.Fatal("decodeFrame accepted a case-variant duplicate key")
	}
	d := &decisionBridge{}
	r := d.decideFrame(bytes.Repeat([]byte("x"), maxHTTPFrame+16))
	if !r.Passthrough || r.Reason != "frame-too-large" {
		t.Fatalf("oversized frame decision = %+v, want passthrough frame-too-large", r)
	}
	if _, err := decodeFrame([]byte(`{"method":"x","params":{"pad":"` + strings.Repeat("a", 2<<20) + `"}}`)); err != nil {
		t.Fatalf("valid 2 MiB frame rejected by decodeFrame: %v", err)
	}
}

// TestFA5PreflightAdvisory: preflight is a shadow. It can add a source-tagged
// metric/audit line after the real decision, but cannot change that decision.
func TestFA5PreflightAdvisory(t *testing.T) {
	resetRouterForTest(t)
	p := preflightVerdict{Reason: "so-pending", Ambiguous: true}
	real := routeResult{Decision: decPin, Reason: "so-admit", Covered: true, ConfBucket: "0.9"}
	var buf bytes.Buffer
	old := metricOut
	metricOut = &buf
	t.Cleanup(func() { metricOut = old })
	recordPreflight(p, real, newPreflightCalibration())
	if real.Decision != decPin || real.Reason != "so-admit" || !real.Covered || real.ConfBucket != "0.9" {
		t.Fatalf("preflight mutated the real decision: %+v", real)
	}
	if !strings.Contains(buf.String(), `"source":"preflight"`) {
		t.Fatalf("shadow metric missing source=preflight: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"confidence"`) {
		t.Fatalf("raw confidence leaked into metric: %s", buf.String())
	}
	path := filepath.Join(os.Getenv("HOME"), routerDir, routerAuditName)
	audit, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audit, []byte(`"source":"preflight"`)) {
		t.Fatalf("shadow audit missing source field: %s", audit)
	}
}

// TestSkipReceiptNamesLayer0: named Layer 0/static outcomes are never chained
// as SO decisions, and emitting them writes no receipt file.
func TestSkipReceiptNamesLayer0(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, r := range []routeResult{
		passResult("layer0-scope", false), passResult("layer0-static", false),
		{Reason: "registry-local"}, {Reason: "ledger-complete"}, {Passthrough: true},
	} {
		if receiptWorthy(r) {
			t.Fatalf("named layer0/static result was receipt-worthy: %+v", r)
		}
	}
	d := &decisionBridge{chain: &receiptChain{}}
	d.emitReceipt(passResult("layer0-static", false), "tools/call", "read_file")
	if _, err := os.Stat(filepath.Join(home, routerDir, "receipt.chain")); !os.IsNotExist(err) {
		t.Fatalf("receipt written for a named layer0 result: %v", err)
	}
	if !receiptWorthy(routeResult{Decision: decPin, Reason: "so-admit"}) {
		t.Fatal("a real SO admit was not receipt-worthy")
	}
}

// TestFA6CacheHitAndNegative covers B11 N16 TTLs, the 8-way split and the
// terminal-negative set. Degraded/uncertain reasons are never stored.
func TestFA6CacheHitAndNegative(t *testing.T) {
	c := newShardedCache()
	cap := soCapability()
	var keys [][32]byte
	seen := map[int]bool{}
	for i := 0; i < 32 && len(seen) < 2; i++ {
		k := gateKeyBytes([]byte(`{"params":{"state":"decide: item `+string(rune('a'+i))+`"}}`), cap)
		idx := int(k[0] & 7)
		if !seen[idx] {
			seen[idx] = true
			keys = append(keys, k)
		}
	}
	if len(keys) < 2 {
		t.Fatal("gateKeyBytes did not span two shards")
	}
	pos, neg := keys[0], keys[1]
	c.store(pos, cachedDecision{Choice: "yes", ConfBucket: "1.0", Reason: "margin=1"})
	c.store(neg, cachedDecision{Abstain: true, Reason: "so-no", ConfBucket: "0.9"})
	if d, ok := c.lookup(pos); !ok || d.Choice != "yes" || d.Abstain {
		t.Fatalf("positive cell miss: %+v ok=%v", d, ok)
	}
	if d, ok := c.lookup(neg); !ok || !d.Abstain || d.Reason != "so-no" {
		t.Fatalf("negative cell miss: %+v ok=%v", d, ok)
	}
	if got := cachedResult(posCell(c, pos).dec); got.Decision != decPin || got.Reason != "cache-hit" {
		t.Fatalf("positive cache result = %+v", got)
	}
	if got := cachedResult(negCell(c, neg).dec); got.Decision != decPass || !got.Abstained {
		t.Fatalf("negative cache result = %+v", got)
	}
	now := time.Now()
	pc := posCell(c, pos)
	nc := negCell(c, neg)
	if d := pc.expiresAt.Sub(now); d < 23*time.Hour || d > 24*time.Hour {
		t.Fatalf("positive TTL = %v, want ~24h", d)
	}
	if d := nc.expiresAt.Sub(now); d < time.Minute || d > time.Hour {
		t.Fatalf("negative TTL = %v, want ~1h", d)
	}
	pc.expiresAt = time.Now().Add(-time.Second)
	if _, ok := c.lookup(pos); ok {
		t.Fatal("expired positive cell still served")
	}
	for _, reason := range []string{"so-degraded", "transient", "unknown", "no-key", "timeout", "so-low-conf-cache-hit"} {
		if terminalNegative[reason] {
			t.Errorf("degraded/uncertain reason %q cached as terminal-negative", reason)
		}
	}
	for _, reason := range []string{"so-low-conf", "so-no", "so-other", "registry-nonproceed"} {
		if !terminalNegative[reason] {
			t.Errorf("terminal reason %q not cached", reason)
		}
	}
}

func posCell(c *shardedCache, k [32]byte) *cacheCell {
	return cellFor(c, k)
}

func negCell(c *shardedCache, k [32]byte) *cacheCell {
	return cellFor(c, k)
}

func cellFor(c *shardedCache, k [32]byte) *cacheCell {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx[hex.EncodeToString(k[:])].Value.(*cacheCell)
}

// TestSOFlightCoalesces proves the shardedCache.lookup path shares one SO call
// across concurrent identical requests (FA7, no semantic tier).
func TestSOFlightCoalesces(t *testing.T) {
	g := newSOFlightGroup()
	var calls int32
	fn := func() routeResult {
		atomic.AddInt32(&calls, 1)
		time.Sleep(20 * time.Millisecond)
		return routeResult{Decision: decPin, Reason: "so-admit"}
	}
	k := gateKeyBytes([]byte(`{"params":{"state":"decide: flight test"}}`), soCapability())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := g.do(k, fn); d.Decision != decPin {
				t.Error("wrong shared decision")
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("SO calls = %d, want 1 coalesced call", got)
	}
}
