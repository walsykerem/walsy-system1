package main

import (
	"bytes"
	"container/list"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	cacheCapacity = 1024
)

// --- canonical state (byte-stable) ---------------------------------------

// volatileKeys are dropped recursively: they change every call without
// changing the decision. Compared in a normalized (case/dash/underscore-free) form.
var volatileKeys = map[string]bool{
	"timestamp": true, "time": true, "ts": true, "date": true,
	"messageref": true, "msgref": true, "messageid": true, "msgid": true,
	"tooloutput": true, "toolresult": true, "toolcallid": true,
	"client": true, "clientname": true, "clientid": true,
	"session": true, "sessionid": true, "requestid": true,
}

func normKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func stripVolatile(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, stripVolatile(e))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if volatileKeys[normKey(k)] {
				continue
			}
			out[k] = stripVolatile(val)
		}
		return out
	default:
		return v
	}
}

// encString goes through Encoder (not json.Marshal) so SetEscapeHTML(false) holds.
func encString(s string) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func canonNumber(raw string) string {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "n:" + raw
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if f == 0 {
		f = 0 // fold -0
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func encodeCanon(v any, b *strings.Builder) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(canonNumber(string(x)))
	case float64:
		b.WriteString(canonNumber(strconv.FormatFloat(x, 'g', -1, 64)))
	case string:
		b.WriteString(encString(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeCanon(e, b)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // sort AFTER canonicalize/strip
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(encString(k))
			b.WriteByte(':')
			encodeCanon(x[k], b)
		}
		b.WriteByte('}')
	default:
		b.WriteString(encString(fmt.Sprint(v)))
	}
}

// foldAlias is the L2 unicode.SimpleFold representative (stdlib has no NFC/NFD).
func foldAlias(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		min := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < min {
				min = n
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

// canonicalState returns a deterministic string. Callers hash it into a key;
// gateKey in decision_bridge.go additionally hashes request bytes and the
// capability config. No map[string]any marshal.
func canonicalState(s string) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		c := "raw:" + encString(s)
		return c + "\x00" + foldAlias(c)
	}
	var b strings.Builder
	encodeCanon(stripVolatile(v), &b)
	c := "v1:" + b.String()
	return c + "\x00" + foldAlias(c)
}

// --- decision cache -------------------------------------------------------

// cachedDecision stores only the typed decision + coarse bucket. Raw conf is
// never held. Label/Steer carry the LLM-backed advice to cache hits, so a hit
// needs no second SO POST and still tells the caller what to do next.
type cachedDecision struct {
	Choice     string
	ConfBucket string
	Reason     string
	Abstain    bool
	Label      string
	Steer      string
}

// cacheTTL admits live 24h; terminal-negative/abstain cells live 1h. Degraded
// and uncertain outcomes are never stored, so they need no TTL.
const (
	cacheTTLPositive = 24 * time.Hour
	cacheTTNNegative = time.Hour
)

type cacheCell struct {
	key       string
	dec       cachedDecision
	expiresAt time.Time
}

type decisionCache struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
	idx map[string]*list.Element
}

func newDecisionCache() *decisionCache {
	return &decisionCache{cap: cacheCapacity, ll: list.New(), idx: make(map[string]*list.Element)}
}

func (c *decisionCache) lookup(key string) (cachedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(key)
}

// lookupLocked requires c.mu. Expired cells are evicted on sight (lazy TTL).
func (c *decisionCache) lookupLocked(key string) (cachedDecision, bool) {
	if el, ok := c.idx[key]; ok {
		cell := el.Value.(*cacheCell)
		if !time.Now().Before(cell.expiresAt) {
			delete(c.idx, key)
			c.ll.Remove(el)
			return cachedDecision{}, false
		}
		c.ll.MoveToFront(el)
		return cell.dec, true
	}
	return cachedDecision{}, false
}

// store writes the cell under the mutex with a lifetime by decision type.
func (c *decisionCache) store(key string, d cachedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeLocked(key, d)
}

// storeLocked mutates idx/ll; the caller must hold c.mu.
func (c *decisionCache) storeLocked(key string, d cachedDecision) {
	ttl := cacheTTLPositive
	if d.Abstain {
		ttl = cacheTTNNegative
	}
	expires := time.Now().Add(ttl)
	if el, ok := c.idx[key]; ok {
		cell := el.Value.(*cacheCell)
		cell.dec, cell.expiresAt = d, expires
		c.ll.MoveToFront(el)
		return
	}
	c.idx[key] = c.ll.PushFront(&cacheCell{key: key, dec: d, expiresAt: expires})
	for c.ll.Len() > c.cap {
		old := c.ll.Back()
		if old == nil {
			return
		}
		delete(c.idx, old.Value.(*cacheCell).key)
		c.ll.Remove(old)
	}
}

// shardedCache splits the LRU across eight fixed shards by the first key byte.
// shortcut: static 8-way split, no dynamic resharding; upgrade only on measured p95 lock contention.
type shardedCache struct {
	shards     [8]*decisionCache
	warmMu     sync.Mutex
	warmHits   map[[32]byte]int
	warmAt     map[[32]byte]time.Time
	warmDec    map[[32]byte]cachedDecision
	refreshing map[[32]byte]bool
}

func newShardedCache() *shardedCache {
	c := &shardedCache{
		warmHits:   make(map[[32]byte]int),
		warmAt:     make(map[[32]byte]time.Time),
		warmDec:    make(map[[32]byte]cachedDecision),
		refreshing: make(map[[32]byte]bool),
	}
	for i := range c.shards {
		c.shards[i] = newDecisionCache()
	}
	return c
}

func (c *shardedCache) shard(k [32]byte) *decisionCache { return c.shards[k[0]&7] }

// needsPrewarm counts every lookup (including the first miss) and fires once
// when a key crosses the hot threshold inside the window.
func (c *shardedCache) needsPrewarm(k [32]byte, threshold int, window time.Duration) bool {
	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	if len(c.warmHits) > 4*cacheCapacity {
		c.warmHits = make(map[[32]byte]int)
		c.warmAt = make(map[[32]byte]time.Time)
		c.warmDec = make(map[[32]byte]cachedDecision)
		c.refreshing = make(map[[32]byte]bool)
	}
	now := time.Now()
	if !c.warmAt[k].Add(window).After(now) {
		c.warmHits[k], c.warmAt[k] = 0, now
		delete(c.warmDec, k)
	}
	c.warmHits[k]++
	if c.warmHits[k] < threshold || c.refreshing[k] {
		return false
	}
	c.refreshing[k] = true
	return true
}

func (c *shardedCache) isHot(k [32]byte, threshold int, window time.Duration) bool {
	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	return c.warmHits[k] >= threshold && c.warmAt[k].Add(window).After(time.Now())
}

func (c *shardedCache) refreshDone(k [32]byte) {
	c.warmMu.Lock()
	delete(c.refreshing, k)
	c.warmMu.Unlock()
}

// lookup is the strict cell read: no warm fallback, so an expired cell is a
// miss for every caller, including direct probes.
func (c *shardedCache) lookup(k [32]byte) (cachedDecision, bool) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(hex.EncodeToString(k[:]))
}

// lookupForDecision keeps hot-key prewarm on the decision path only; the
// fallback is bounded by a recent hot hit, not by wall-clock pin lifetime.
func (c *shardedCache) lookupForDecision(k [32]byte) (cachedDecision, bool) {
	hot := c.needsPrewarm(k, 3, 30*time.Second)
	s := c.shard(k)
	key := hex.EncodeToString(k[:])
	s.mu.Lock()
	d, hit := s.lookupLocked(key)
	if hit {
		s.mu.Unlock()
		c.warmMu.Lock()
		c.warmDec[k] = d
		c.warmMu.Unlock()
		if hot {
			// Async TTL refresh from a lookup hit: no second SO POST.
			go func(kk [32]byte, dd cachedDecision) {
				c.store(kk, dd)
				c.refreshDone(kk)
			}(k, d)
		}
		return d, true
	}
	// threshold+1: the crossing lookup itself must still observe expiry (N16 TTL).
	if c.isHot(k, 4, 30*time.Second) {
		c.warmMu.Lock()
		warm, ok := c.warmDec[k]
		c.warmMu.Unlock()
		if ok {
			s.storeLocked(key, warm)
			s.mu.Unlock()
			c.refreshDone(k) // no async refresh was launched on this warm hit
			return warm, true
		}
	}
	s.mu.Unlock()
	if hot {
		c.refreshDone(k)
	}
	return cachedDecision{}, false
}

func (c *shardedCache) store(k [32]byte, d cachedDecision) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeLocked(hex.EncodeToString(k[:]), d)
}
