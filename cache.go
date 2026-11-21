package main

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const (
	abstainLabel  = "abstain"
	admitMargin   = 0.10 // clef is overconfident (0.97–1.0): require an argmax gap
	canaryEvery   = 8    // 1/N replay of known-label cells
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

// canonicalState returns a deterministic string; the caller hashes it together
// with rubricVersion/model/conf-rule/registryVersion. No map[string]any marshal.
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
// never held. Reason may carry "margin=<float>" for admission.
type cachedDecision struct {
	Choice     string
	ConfBucket string
	Reason     string
}

type cacheCell struct {
	key string
	dec cachedDecision
}

// cacheStats is the ROUTER decision-cache ledger only.
type cacheStats struct {
	Hits           int
	Misses         int
	ReusePrecision int
	WrongReuse     int
}

// providerKVStats is the PROVIDER prefix-cache ledger. Deliberately a separate
// type and a separate decisionCache field — never merged with cacheStats.
type providerKVStats struct {
	MatchPrefix int
	PrefixLen   int
	PromptLen   int
}

type decisionCache struct {
	mu      sync.Mutex
	cap     int
	ll      *list.List
	idx     map[string]*list.Element
	router  cacheStats
	kv      providerKVStats
	flushes int
}

func newDecisionCache() *decisionCache {
	return &decisionCache{cap: cacheCapacity, ll: list.New(), idx: make(map[string]*list.Element)}
}

func (c *decisionCache) lookup(key string) (cachedDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.ll.MoveToFront(el)
		c.router.Hits++
		return el.Value.(*cacheCell).dec, true
	}
	c.router.Misses++
	return cachedDecision{}, false
}

func (c *decisionCache) store(key string, d cachedDecision) {
	if el, ok := c.idx[key]; ok {
		el.Value.(*cacheCell).dec = d
		c.ll.MoveToFront(el)
		return
	}
	c.idx[key] = c.ll.PushFront(&cacheCell{key: key, dec: d})
	for c.ll.Len() > c.cap {
		old := c.ll.Back()
		if old == nil {
			return
		}
		delete(c.idx, old.Value.(*cacheCell).key)
		c.ll.Remove(old)
	}
}

func validBucket(b string) bool {
	return b == "0.8" || b == "0.9" || b == "1.0"
}

func reasonMargin(reason string) (float64, bool) {
	const p = "margin="
	if !strings.HasPrefix(reason, p) {
		return 0, false
	}
	m, err := strconv.ParseFloat(strings.TrimPrefix(reason, p), 64)
	return m, err == nil
}

// admit: negative verdicts are always cached as abstain (skip repeat SO calls);
// positive labels need a valid bucket AND a margin >= admitMargin.
func (c *decisionCache) admit(key string, d cachedDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if d.Choice == "no" || d.Choice == "other" || !validBucket(d.ConfBucket) {
		c.store(key, cachedDecision{Choice: abstainLabel, ConfBucket: d.ConfBucket, Reason: "negative-verdict"})
		return
	}
	m, ok := reasonMargin(d.Reason)
	if !ok {
		m = 1 // caller supplied no margin signal -> full separation assumed
	}
	if m < admitMargin {
		return
	}
	c.store(key, d)
}

func (c *decisionCache) flush(key string) {
	if el, ok := c.idx[key]; ok {
		delete(c.idx, key)
		c.ll.Remove(el)
	}
}

func shouldCanaryReplay(n int) bool { return n > 0 && n%canaryEvery == 0 }

// canaryReplay: 1/N probe of a known-label cell. Mismatch -> cell flush + counters.
func (c *decisionCache) canaryReplay(key, want string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.idx[key]
	if !ok || d.Value.(*cacheCell).dec.Choice != want {
		if ok {
			c.flush(key)
		}
		c.router.WrongReuse++
		c.flushes++
		return false
	}
	c.router.ReusePrecision++
	return true
}

func (c *decisionCache) canaryFlushes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushes
}

func (c *decisionCache) stats() cacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.router
}

// recordProviderPrefix touches ONLY the provider KV ledger.
func (c *decisionCache) recordProviderPrefix(prefixLen, promptLen int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kv.MatchPrefix++
	if prefixLen > c.kv.PrefixLen {
		c.kv.PrefixLen = prefixLen
	}
	if promptLen > c.kv.PromptLen {
		c.kv.PromptLen = promptLen
	}
}

func (c *decisionCache) providerStats() providerKVStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kv
}

// --- singleflight (~15 lines of stdlib) ------------------------------------

type groupCall struct {
	wg  sync.WaitGroup
	val cachedDecision
	err error
}

type group struct {
	mu sync.Mutex
	m  map[string]*groupCall
}

// finish is identity-checked: a stale leader cannot delete a newer call.
func (g *group) finish(key string, c *groupCall) {
	g.mu.Lock()
	if g.m[key] == c {
		delete(g.m, key)
	}
	g.mu.Unlock()
}

// do coalesces concurrent callers on one key. Followers read val/err only after
// wg.Wait(). A leader panic becomes an error; a follower's ctx cannot poison it.
func (g *group) do(key string, fn func() (cachedDecision, error)) (val cachedDecision, err error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*groupCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &groupCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			c.err = fmt.Errorf("singleflight panic: %v", r)
			err = c.err
		}
		c.wg.Done()
		g.finish(key, c)
	}()
	c.val, c.err = fn()
	return c.val, c.err
}

// forget drops a leader that exceeded the 1500ms budget; its later finish is
// harmless because finish() is identity-checked.
func (g *group) forget(key string) {
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
}
